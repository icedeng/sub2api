package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Wei-Shaw/sub2api/internal/wayviaegress"
)

func main() {
	path := flag.String("config", "/etc/wayvia-egress/config.json", "root管理的固定配置")
	flag.Parse()
	info, err := os.Lstat(*path)
	if err != nil {
		log.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || !info.Mode().IsRegular() {
		log.Fatal("配置必须为root拥有的普通文件")
	}
	if info.Mode().Perm()&0022 != 0 {
		log.Fatal("配置不能被组或其他用户写入")
	}
	c, err := wayviaegress.ReadConfig(*path)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err = wayviaegress.RunAgent(ctx, c); err != nil {
		log.Fatal(err)
	}
}
