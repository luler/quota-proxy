package bin

import (
	"context"
	"fmt"
	"gin_base/app/config"
	"gin_base/route"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/gin-gonic/gin"
	"github.com/spf13/cobra"
)

func ServeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "启动配额中间件服务",
		Run: func(cmd *cobra.Command, args []string) {
			StartServer()
		},
	}

	return cmd
}

// 开启gin服务
func StartServer() {
	// 获取配置
	cfg := config.GetConfig()
	if cfg == nil {
		fmt.Println("配置未加载，请检查配置文件")
		os.Exit(1)
	}

	// 设置 Gin 模式
	gin.SetMode(os.Getenv(gin.EnvGinMode))

	engine := gin.Default()

	// 初始化路由和中间件
	if err := route.InitRouter(engine, cfg); err != nil {
		fmt.Println("初始化路由失败:", err)
		os.Exit(1)
	}

	// 自定义端口
	port := os.Getenv("PORT")
	if port == "" {
		port = fmt.Sprintf("%d", cfg.Server.Port)
	}

	// 启动服务
	fmt.Printf("配额中间件服务启动，监听端口: %s\n", port)
	fmt.Printf("上游服务地址: %s\n", cfg.Upstream.Target)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           engine,
		ReadHeaderTimeout: cfg.Server.ReadTimeout,
		IdleTimeout:       cfg.Server.IdleTimeout,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Println("服务启动失败:", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	fmt.Println("正在关闭服务...")

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ReadTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		fmt.Println("服务关闭异常:", err)
	}
}
