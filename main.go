package main

import (
	"github.com/universonic/xiaoya-emby/engine"
)

var (
	cfg = new(engine.Config)
)

func main() {
	cfg.Command().Execute()
}
