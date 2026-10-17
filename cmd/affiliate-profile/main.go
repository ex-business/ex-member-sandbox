package main

import (
	"context"
	"encoding/json"
	"ex-member-sandbox/internal/app"
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	config := flag.String("config", "", "模拟会员配置")
	out := flag.String("out", "", "私有展示快照文件")
	interval := flag.Duration("interval", 0, "刷新间隔，0为单次")
	flag.Parse()
	if *interval < 0 || *interval > 0 && *interval < time.Second {
		fmt.Fprintln(os.Stderr, "刷新间隔至少1秒")
		os.Exit(1)
	}
	raw, e := os.ReadFile(*config)
	if e != nil {
		panic("会员配置不可用")
	}
	var c struct{ Database, Tenant, App, Source string }
	if json.Unmarshal(raw, &c) != nil || c.Database == "" || c.Tenant == "" || c.App == "" || c.Source == "" || *out == "" {
		panic("展示导出配置无效")
	}
	store, e := app.Open(c.Database)
	if e != nil {
		panic("会员库不可用")
	}
	defer store.DB.Close()
	var lastError time.Time
	for {
		if e = store.ExportAffiliateProfiles(context.Background(), *out, c.Tenant, c.App, c.Source); e != nil {
			if lastError.IsZero() || time.Since(lastError) >= time.Minute {
				fmt.Fprintln(os.Stderr, "代理展示快照导出未完成:", e)
				lastError = time.Now()
			}
			if *interval == 0 {
				os.Exit(1)
			}
		}
		if *interval == 0 {
			return
		}
		time.Sleep(*interval)
	}
}
