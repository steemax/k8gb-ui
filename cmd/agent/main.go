package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8gb-ui/internal/agent"
	"k8gb-ui/internal/auditlog"
	"k8gb-ui/internal/dnszones"
	"k8gb-ui/internal/mutation"
	"k8gb-ui/internal/traefik"
	"k8s.io/apimachinery/pkg/util/validation"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("agent", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:8081", "HTTP listen address")
	clusterID := flags.String("cluster-id", "", "Stable cluster ID (required)")
	kubeconfig := flags.String("kubeconfig", "", "Explicit kubeconfig path; empty uses in-cluster config")
	tokenFile := flags.String("token-file", "", "File containing a bearer token of at least 32 characters")
	insecure := flags.Bool("insecure-local", false, "Disable bearer auth on a literal loopback address only")
	allowed := flags.String("allowed-namespaces", "", "Comma-separated namespace allowlist; empty lists all namespaces")
	collectionTimeout := flags.Duration("snapshot-timeout", 30*time.Second, "Maximum time for one collection")
	requestTimeout := flags.Duration("request-timeout", 5*time.Second, "Maximum time for one Kubernetes request")
	pageSize := flags.Int64("page-size", 500, "Kubernetes list page size (1-1000)")
	maxPages := flags.Int("max-pages", 100, "Maximum pages per resource (1-1000)")
	maxConcurrent := flags.Int("max-concurrent-requests", 8, "Maximum concurrent collections (1-128)")
	enableWrites := flags.Bool("enable-writes", false, "Explicitly enable administrator mutation API")
	nativeSiteTraffic := flags.Bool("enable-native-site-traffic", false, "Opt in to native site exclusion by changing the existing Ingress; LOCAL routing also stops while excluded (requires --enable-writes)")
	auditConfigMap := flags.String("audit-configmap", "", "Opt in to a shared 700 KiB journal in this pre-provisioned ConfigMap name in each application namespace; empty keeps replica-local memory")
	auditTimeout := flags.Duration("audit-timeout", 2*time.Second, "Maximum time for an audit persistence/read attempt (100ms-10s)")
	writeTokenFile := flags.String("write-token-file", "", "Dedicated write bearer token file (not the read token)")
	planKeyFile := flags.String("plan-key-file", "", "Dedicated plan signing key file (at least 32 random bytes)")
	geotags := flags.String("geotags", "", "Comma-separated allowed GeoTags for this DNS zone")
	adapter := flags.Bool("traefik-adapter", false, "Enable optional Traefik IngressRoute catalog/provider")
	controllerNS := flags.String("k8gb-namespace", "k8gb", "Namespace of the controller Deployment used for DNS zone discovery")
	controllerName := flags.String("k8gb-deployment", "k8gb", "Named controller Deployment; GET only, no Pod or Secret reads")
	controllerContainer := flags.String("k8gb-container", "", "Controller container name, required if multiple containers declare DNS zones")
	domains := flags.String("traefik-domains", "", "Additional allowed global DNS suffixes (CSV); match webhook --domains")
	domain := flags.String("traefik-domain", "", "Configured global DNS suffix, or require explicit gslb_hostname annotations")
	localDomain := flags.String("traefik-local-domain", "", "Configured local DNS suffix")
	ingressClass := flags.String("traefik-ingress-class", "traefik", "Generated native Ingress class")
	tlsOption := flags.String("traefik-mtls-tls-option", "", "Existing Traefik TLSOption reference for client authentication")
	mtlsEntry := flags.String("traefik-mtls-entrypoint", "mtls", "Legacy default mTLS entrypoint; prefer explicit per-route selection")
	mtlsEntries := flags.String("traefik-mtls-entrypoints", "", "Approved existing TLS entrypoints (CSV); identical on all sites")
	ordinaryEntries := flags.String("traefik-ordinary-entrypoints", "websecure,grpc,grpctls", "Approved ordinary entrypoints (CSV)")
	tlsNamespaces := flags.String("traefik-tls-option-namespaces", "", "Approved TLSOption namespaces (CSV); empty uses the application namespace")
	cnPlugin := flags.String("traefik-cn-plugin", "", "Opt-in installed famedly/traefik-certauthz alias; filters DNS SAN, not Subject CN")
	blocking := flags.String("traefik-blocking-middleware", "", "Optional middleware identifying a blocking first route")
	webhookConfig := flags.String("traefik-webhook-config", "k8gb-ui-traefik", "Named side-effect-free fail-closed MutatingWebhookConfiguration")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if strings.TrimSpace(*clusterID) == "" {
		return errors.New("--cluster-id is required")
	}
	if *nativeSiteTraffic && !*enableWrites {
		return errors.New("--enable-native-site-traffic requires --enable-writes")
	}
	if *auditConfigMap != "" && len(validation.IsDNS1123Subdomain(*auditConfigMap)) != 0 {
		return errors.New("--audit-configmap must be a valid ConfigMap name")
	}
	if *auditTimeout < 100*time.Millisecond || *auditTimeout > 10*time.Second {
		return errors.New("--audit-timeout must be between 100ms and 10s")
	}
	if *insecure && *tokenFile != "" {
		return errors.New("--token-file and --insecure-local are mutually exclusive")
	}
	serverOpts := agent.ServerOptions{ListenAddress: *listen, InsecureLocal: *insecure, MaxConcurrentRequests: *maxConcurrent}
	if !*insecure {
		if *tokenFile == "" {
			return errors.New("--token-file is required unless --insecure-local is explicitly enabled")
		}
		token, err := agent.LoadToken(*tokenFile)
		if err != nil {
			return fmt.Errorf("read bearer token: %w", err)
		}
		serverOpts.Token = token
	}
	if err := agent.ValidateServerOptions(serverOpts); err != nil {
		return err
	}
	namespaceList := []string{}
	if *allowed != "" {
		for _, ns := range strings.Split(*allowed, ",") {
			namespaceList = append(namespaceList, strings.TrimSpace(ns))
		}
	}
	client, err := agent.NewKubernetesClient(*kubeconfig, *requestTimeout)
	if err != nil {
		return fmt.Errorf("configure Kubernetes client: %w", err)
	}
	zoneReader := &dnszones.Reader{Client: client, Config: dnszones.Config{Namespace: *controllerNS, Deployment: *controllerName, Container: *controllerContainer}}
	if err := zoneReader.Config.Validate(); err != nil {
		return err
	}
	collector, err := agent.NewCollector(client, agent.Options{DNSZones: zoneReader, ClusterID: *clusterID, AllowedNamespaces: namespaceList, SnapshotTimeout: *collectionTimeout, RequestTimeout: *requestTimeout, PageSize: *pageSize, MaxPages: *maxPages})
	if err != nil {
		return err
	}
	tags := []string{}
	for _, tag := range strings.Split(*geotags, ",") {
		if strings.TrimSpace(tag) != "" {
			tags = append(tags, strings.TrimSpace(tag))
		}
	}
	providers := map[string]mutation.Provider{"native": mutation.NewNativeProvider(client, tags), "traffic": mutation.NewTrafficProvider(client)}
	if *nativeSiteTraffic {
		providers["native"] = mutation.NewNativeProviderWithSiteTraffic(client, tags)
	}
	if *adapter {
		cfg := traefik.Config{Domain: *domain, LocalDomain: *localDomain, IngressClass: *ingressClass, MTLSTLSOption: *tlsOption, CNPlugin: *cnPlugin, BlockingMiddleware: *blocking, AllowedGeoTags: tags, AllowedNamespaces: namespaceList, WebhookConfig: *webhookConfig}
		cfg.MTLSEntrypoint = *mtlsEntry
		for _, pair := range []struct {
			raw    string
			target *[]string
		}{{*mtlsEntries, &cfg.MTLSEntrypoints}, {*ordinaryEntries, &cfg.OrdinaryEntrypoints}, {*tlsNamespaces, &cfg.TLSOptionNamespaces}} {
			if pair.raw != "" {
				for _, value := range strings.Split(pair.raw, ",") {
					*pair.target = append(*pair.target, strings.TrimSpace(value))
				}
			}
		}
		if *domains != "" {
			for _, d := range strings.Split(*domains, ",") {
				cfg.Domains = append(cfg.Domains, strings.TrimSpace(d))
			}
		}
		if err := traefik.ValidateConfig(cfg); err != nil {
			return err
		}
		serverOpts.TraefikConfig = &cfg
		providers["traefik"] = traefik.NewProvider(client, cfg)
	}
	var signingKey []byte
	if *enableWrites {
		if *writeTokenFile == "" || *planKeyFile == "" || len(tags) == 0 {
			return errors.New("writes require --write-token-file, --plan-key-file and explicit --geotags")
		}
		serverOpts.WriteToken, err = agent.LoadToken(*writeTokenFile)
		if err != nil {
			return err
		}
		signingKey, err = os.ReadFile(*planKeyFile)
		if err != nil {
			return err
		}
		if len(signingKey) < 32 || bytes.Equal(bytes.TrimSpace(signingKey), []byte(serverOpts.Token)) || bytes.Equal(bytes.TrimSpace(signingKey), []byte(serverOpts.WriteToken)) {
			return errors.New("plan signing key must be dedicated and at least 32 bytes")
		}
	}
	var store auditlog.Journal = auditlog.New()
	if *auditConfigMap != "" {
		shared, err := auditlog.NewConfigMap(client, auditlog.ConfigMapOptions{ClusterID: *clusterID, Name: *auditConfigMap, AllowedNamespaces: namespaceList, Timeout: *auditTimeout})
		if err != nil {
			return err
		}
		for _, ns := range namespaceList {
			if err := shared.Check(ctx, ns); err != nil {
				return fmt.Errorf("check audit ConfigMap in %s: %w", ns, err)
			}
		}
		store = shared
		retryCtx, stopRetries := context.WithCancel(ctx)
		retriesDone := make(chan struct{})
		go func() { defer close(retriesDone); shared.Run(retryCtx) }()
		defer func() {
			stopRetries()
			<-retriesDone
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shared.Flush(flushCtx); err != nil {
				log.Printf("audit final flush failed: %v", err)
			}
		}()
	}
	serverOpts.Audit = store
	serverOpts.Mutations, err = mutation.NewManager(client, mutation.Options{DNSZones: zoneReader, ClusterID: *clusterID, Enabled: *enableWrites, SigningKey: signingKey, AllowedNamespaces: namespaceList, Providers: providers, Audit: store.Append})
	if err != nil {
		return err
	}
	handler, err := agent.NewHandler(collector, serverOpts)
	if err != nil {
		return err
	}
	server := agent.NewHTTPServer(*listen, handler, *collectionTimeout)
	finished := make(chan error, 1)
	go func() { finished <- server.ListenAndServe() }()
	log.Printf("Kubernetes agent listening on %s writes_enabled=%v", *listen, *enableWrites)
	select {
	case err := <-finished:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			<-finished
			return err
		}
		<-finished
		return nil
	}
}
