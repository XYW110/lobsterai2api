// main.go lobsterai2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"lobsterai2api/internal/auth"
	"lobsterai2api/internal/pool"
	"lobsterai2api/internal/scheduler"
	"lobsterai2api/internal/server"
	"lobsterai2api/internal/upstream"
)

// authRescanInterval auths/ 目录重扫间隔（容器里新增账号后免重启）。
const authRescanInterval = 30 * time.Second

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// 配置文件不存在时给一次机会用纯默认 + env（容器部署不挂载 config.json 是常态）
		if errors.Is(err, fs.ErrNotExist) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	// 上游基址统一由 upstream 包收口：config.json 的 upstream.base_url 优先
	if cfg.Upstream.BaseURL != "" {
		upstream.SetServerBase(cfg.Upstream.BaseURL)
	}
	if upstream.ServerBase() == "" {
		log.Printf("warning: upstream base url is empty (set LB2A_UPSTREAM_BASE or upstream.base_url)")
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	p := pool.New(cfg.StateFile)
	for _, a := range auths {
		p.Add(a)
	}

	up := upstream.New()
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second

	sch := scheduler.New(scheduler.Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   cfg.Schedule.CheckinHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
	})

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		HardCooldown: cfg.HardCreditDur,
		SoftCooldown: cfg.SoftRateDur,
		ErrThreshold: cfg.Cooldown.ErrThresh,
		ErrCooldown:  cfg.ErrCooldownDur,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)
	go rescanAuths(ctx, p, cfg.AuthDir)
	// 启动后延迟拉一次积分（立即刷新 pool.credits，不用等整点）
	go func() {
		time.Sleep(5 * time.Second)
		sch.RunCheckinNow()
	}()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("lobsterai2api listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// rescanAuths 周期性重扫 auths/ 目录并同步进池：
// 新增账号文件自动生效、被删除的账号自动剔除（凭证刷新落盘后不受影响）。
func rescanAuths(ctx context.Context, p *pool.Pool, dir string) {
	t := time.NewTicker(authRescanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			auths, err := auth.LoadDir(dir)
			if err != nil {
				continue
			}
			p.SyncToDir(auths)
		}
	}
}
