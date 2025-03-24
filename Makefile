NAME=xiaoya-emby
BINDIR=bin

GOBUILD=go build -tags with_gvisor -trimpath -ldflags '-X "github.com/universonic/xiaoya-emby/engine.Version=$(VERSION)" \
		-w -s -buildid='

all:linux-amd64 linux-arm64\
	darwin-amd64 darwin-arm64\
 	windows-amd64 windows-arm64\

darwin-amd64:
	GOARCH=amd64 GOOS=darwin $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

darwin-arm64:
	GOARCH=arm64 GOOS=darwin $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

linux-amd64:
	GOARCH=amd64 GOOS=linux $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

linux-arm64:
	GOARCH=arm64 GOOS=linux $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

windows-amd64:
	GOARCH=amd64 GOOS=windows $(GOBUILD) -o $(BINDIR)/$(NAME)-$@.exe

windows-arm64:
	GOARCH=arm64 GOOS=windows $(GOBUILD) -o $(BINDIR)/$(NAME)-$@.exe