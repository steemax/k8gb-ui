package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"k8gb-ui/internal/traefik"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func csv(s string) []string {
	if s == "" {
		return nil
	}
	values := strings.Split(s, ",")
	for i := range values {
		values[i] = strings.TrimSpace(values[i])
	}
	return values
}

func run(ctx context.Context, args []string) error {
	f := flag.NewFlagSet("webhook", flag.ContinueOnError)
	listen := f.String("listen", ":8443", "HTTPS listen address")
	cert := f.String("tls-cert-file", "", "PEM TLS certificate (required)")
	key := f.String("tls-private-key-file", "", "PEM TLS private key (required)")
	insecure := f.Bool("insecure-local", false, "TEST ONLY: HTTP on a literal loopback address")
	reconcile := f.Bool("reconcile", false, "Also periodically reconcile committed parents")
	kubeconfig := f.String("kubeconfig", "", "Explicit kubeconfig; empty uses in-cluster config when reconciling")
	interval := f.Duration("reconcile-interval", 30*time.Second, "Reconciliation interval (1s..24h)")
	namespaces := f.String("allowed-namespaces", "", "Namespace allowlist; required for reconciliation")
	geotags := f.String("allowed-geo-tags", "", "Allowed primary/weighted geo tags (CSV)")
	ordinary := f.String("ordinary-entrypoints", "websecure,grpc,grpctls", "Default derived ordinary Ingress entrypoints")
	mtlsEntries := f.String("mtls-entrypoints", "", "Approved existing TLS entrypoints (CSV); match agent --traefik-mtls-entrypoints")
	tlsNamespaces := f.String("tls-option-namespaces", "", "Approved TLSOption namespaces (CSV); empty uses the application namespace")
	var cfg traefik.Config
	domains := f.String("domains", "", "Additional allowed global DNS suffixes (CSV); match agent --traefik-domains")
	f.StringVar(&cfg.Domain, "domain", "", "Require generated hostnames below this domain; no first-label inference")
	f.StringVar(&cfg.LocalDomain, "local-domain", "", "Optional domain for local children")
	f.StringVar(&cfg.IngressClass, "ingress-class", "", "Optional generated Ingress class")
	f.StringVar(&cfg.MTLSEntrypoint, "mtls-entrypoint", "mtls", "mTLS entrypoint")
	f.StringVar(&cfg.MTLSTLSOption, "mtls-tls-option", "", "Explicit Traefik TLS option reference")
	f.StringVar(&cfg.CNPlugin, "cn-plugin", "", "Opt-in installed famedly/traefik-certauthz alias; filters DNS SAN, not Subject CN")
	f.StringVar(&cfg.BlockingMiddleware, "blocking-middleware", "", "Optional legacy blocking middleware selecting route 1")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	cfg.AllowedNamespaces, cfg.AllowedGeoTags, cfg.OrdinaryEntrypoints = csv(*namespaces), csv(*geotags), csv(*ordinary)
	cfg.Domains = csv(*domains)
	cfg.MTLSEntrypoints, cfg.TLSOptionNamespaces = csv(*mtlsEntries), csv(*tlsNamespaces)
	if err := traefik.ValidateConfig(cfg); err != nil {
		return err
	}
	if *interval < time.Second || *interval > 24*time.Hour {
		return fmt.Errorf("reconcile interval must be 1s..24h")
	}
	if *insecure {
		host, _, err := net.SplitHostPort(*listen)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || !ip.IsLoopback() || *cert != "" || *key != "" {
			return fmt.Errorf("--insecure-local requires a literal loopback listen address and no TLS files")
		}
	} else if *cert == "" || *key == "" {
		return fmt.Errorf("--tls-cert-file and --tls-private-key-file are required")
	}
	mtlsConfigured := len(traefik.MTLSEntrypoints(cfg)) > 0
	if !*reconcile && !mtlsConfigured && *kubeconfig != "" {
		return fmt.Errorf("--kubeconfig is only used with --reconcile or configured mTLS validation")
	}
	var controller *traefik.Reconciler
	var validationClient dynamic.Interface
	if *reconcile || mtlsConfigured {
		if *reconcile && len(cfg.AllowedNamespaces) == 0 {
			return fmt.Errorf("--reconcile requires explicit --allowed-namespaces")
		}
		var kc *rest.Config
		var err error
		if *kubeconfig != "" {
			kc, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
		} else {
			kc, err = rest.InClusterConfig()
		}
		if err != nil {
			return fmt.Errorf("configure reconciler client: %w", err)
		}
		kc.Timeout = 5 * time.Second
		kc.QPS = 10
		kc.Burst = 20
		client, err := dynamic.NewForConfig(kc)
		if err != nil {
			return err
		}
		validationClient = client
		if *reconcile {
			controller = &traefik.Reconciler{Client: client, Config: cfg}
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/mutate", traefik.NewValidatedAdmissionHandler(validationClient, cfg))
	// Readiness describes the HTTP admission server, not per-policy Kubernetes
	// reachability or successful reconciliation. Runtime failures are separate.
	health := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	}
	mux.HandleFunc("/readyz", health)
	mux.HandleFunc("/healthz", health)
	server := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	// Bind and load TLS before launching the loop; startup failure cannot leave a controller running.
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	if !*insecure {
		pair, err := tls.LoadX509KeyPair(*cert, *key)
		if err != nil {
			_ = listener.Close()
			return err
		}
		server.TLSConfig.Certificates = []tls.Certificate{pair}
		listener = tls.NewListener(listener, server.TLSConfig)
	}
	loopCtx, stop := context.WithCancel(ctx)
	defer stop()
	var workers sync.WaitGroup
	if controller != nil {
		workers.Add(1)
		go func() {
			defer workers.Done()
			timer := time.NewTicker(*interval)
			defer timer.Stop()
			for {
				if err := controller.Sweep(loopCtx); err != nil && loopCtx.Err() == nil {
					log.Printf("reconciliation failed: %v", err)
				}
				select {
				case <-loopCtx.Done():
					return
				case <-timer.C:
				}
			}
		}()
	}
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	log.Printf("admission listening on %s (reconciler=%t)", listener.Addr(), controller != nil)
	select {
	case err := <-finished:
		stop()
		workers.Wait()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := server.Shutdown(shutdownCtx)
		if err != nil {
			_ = server.Close()
		}
		<-finished
		workers.Wait()
		return err
	}
}
