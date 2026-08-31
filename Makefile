NAME=xiaoya-emby
BINDIR=bin
VERSION=v0.2.0

GOBUILD=CGO_ENABLED=1 go build -tags with_gvisor -trimpath -ldflags '-X "github.com/universonic/xiaoya-emby/engine.Version=$(VERSION)" \
		-w -s -buildid='

.PHONY: all darwin-amd64 darwin-arm64 linux-amd64 linux-arm64

all:linux-amd64 linux-arm64 \
	darwin-amd64 darwin-arm64

darwin-amd64 darwin-arm64 linux-amd64 linux-arm64: | $(BINDIR)

$(BINDIR):
	mkdir -p "$@"

darwin-amd64:
	GOARCH=amd64 GOOS=darwin $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

darwin-arm64:
	GOARCH=arm64 GOOS=darwin $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

linux-amd64:
	GOARCH=amd64 GOOS=linux $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

linux-arm64:
	GOARCH=arm64 GOOS=linux $(GOBUILD) -o $(BINDIR)/$(NAME)-$@
