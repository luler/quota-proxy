package main

import (
	"fmt"
	"gin_base/app"
	"gin_base/bin"
	"os"

	"github.com/spf13/cobra"
)

func init() {
	// 项目初始化
	app.InitApp(app.InitTypeBase)
}

func main() {
	cmd := &cobra.Command{
		Use:   "quota_middleware",
		Short: "成功访问次数限制中间件",
		Long:  "成功访问次数限制中间件 - 作为反向代理实现按时间窗口配额控制",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("请使用子命令，或添加 --help 查看帮助")
		},
	}

	cmd.AddCommand(bin.ServeCommand())

	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
