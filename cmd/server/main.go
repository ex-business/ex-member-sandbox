package main

import (
	"context"
	"encoding/json"
	"ex-member-sandbox/internal/app"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Config struct{ Listen, PublicURL, Database, CommissionURL, CommissionToken, RegistrationKey, Tenant, App, Source string }

func main() {
	configFile := flag.String("config", ".local/config.json", "本地私有配置文件")
	flag.Parse()
	// 本项目只启动回环沙箱；配置、数据库及日志默认仅当前用户可读，禁止意外暴露会员数据。
	syscall.Umask(0077)
	raw, err := os.ReadFile(*configFile)
	if err != nil {
		slog.Error("读取本地配置失败")
		os.Exit(1)
	}
	var cfg Config
	if json.Unmarshal(raw, &cfg) != nil {
		slog.Error("本地配置格式无效")
		os.Exit(1)
	}
	host, _, err := net.SplitHostPort(cfg.Listen)
	ip := net.ParseIP(host)
	public, e1 := url.Parse(cfg.PublicURL)
	upstream, e2 := url.Parse(cfg.CommissionURL)
	if err != nil || ip == nil || !ip.IsLoopback() || e1 != nil || e2 != nil || public.Scheme != "http" || public.Host != cfg.Listen || public.Path != "/" || public.RawQuery != "" || public.Fragment != "" || public.User != nil || upstream.Scheme != "http" || net.ParseIP(upstream.Hostname()) == nil || !net.ParseIP(upstream.Hostname()).IsLoopback() || upstream.User != nil || upstream.RawQuery != "" || upstream.Fragment != "" || upstream.Path != "" || len(cfg.CommissionToken) < 32 || len(cfg.RegistrationKey) < 32 || cfg.Tenant == "" || cfg.App == "" || cfg.Source == "" {
		slog.Error("配置无效：仅支持完整的本地回环地址及受控来源")
		os.Exit(1)
	}
	if err = os.MkdirAll(filepath.Dir(cfg.Database), 0700); err != nil {
		slog.Error("创建本地数据目录失败")
		os.Exit(1)
	}
	store, err := app.Open(cfg.Database)
	if err != nil {
		slog.Error("打开独立会员数据库失败", "error", err)
		os.Exit(1)
	}
	defer store.DB.Close()
	client := &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	commission := &app.Commission{URL: strings.TrimRight(cfg.CommissionURL, "/"), Token: cfg.CommissionToken, Key: cfg.RegistrationKey, Tenant: cfg.Tenant, App: cfg.App, Source: cfg.Source, Client: client}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err = app.Serve(ctx, cfg.Listen, app.NewServer(store, commission, cfg.PublicURL)); err != nil {
		slog.Error("临时会员服务退出", "error", err)
		os.Exit(1)
	}
}
