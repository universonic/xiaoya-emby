NAME=xiaoya-emby
BINDIR=bin
VERSION=v0.1.0

GOBUILD=go build -tags with_gvisor -trimpath -ldflags '-X "github.com/universonic/xiaoya-emby/engine.Version=$(VERSION)" \
		-w -s -buildid='

all:linux-amd64 linux-arm64 \
	darwin-amd64 darwin-arm64

darwin-amd64:
	GOARCH=amd64 GOOS=darwin $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

darwin-arm64:
	GOARCH=arm64 GOOS=darwin $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

linux-amd64:
	GOARCH=amd64 GOOS=linux $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

linux-arm64:
	GOARCH=arm64 GOOS=linux $(GOBUILD) -o $(BINDIR)/$(NAME)-$@
