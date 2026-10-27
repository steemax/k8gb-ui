.PHONY: build test vulncheck demo images image-smoke chart

IMAGE_REPOSITORY ?= steemax/k8gb-ui
VERSION ?= 0.1.2

build:
	go build -trimpath -o bin/console ./cmd/console
	go build -trimpath -o bin/agent ./cmd/agent
	go build -trimpath -o bin/webhook ./cmd/webhook
	go build -trimpath -o bin/password-hash ./cmd/password-hash

test:
	go test -race ./...
	node --test tests/frontend*.test.cjs

vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

demo:
	go run ./cmd/console --demo

images:
	docker build --build-arg COMPONENT=console --build-arg VERSION=$(VERSION) -t $(IMAGE_REPOSITORY):$(VERSION)-console .
	docker build --build-arg COMPONENT=agent --build-arg VERSION=$(VERSION) -t $(IMAGE_REPOSITORY):$(VERSION)-agent .
	docker build --build-arg COMPONENT=webhook --build-arg VERSION=$(VERSION) -t $(IMAGE_REPOSITORY):$(VERSION)-webhook .

image-smoke: images
	docker run --rm --network=none --read-only --cap-drop=ALL --security-opt=no-new-privileges $(IMAGE_REPOSITORY):$(VERSION)-console --help
	docker run --rm --network=none --read-only --cap-drop=ALL --security-opt=no-new-privileges $(IMAGE_REPOSITORY):$(VERSION)-agent --help
	docker run --rm --network=none --read-only --cap-drop=ALL --security-opt=no-new-privileges $(IMAGE_REPOSITORY):$(VERSION)-webhook --help

chart:
	helm package charts/k8gb-ui --destination dist/charts
