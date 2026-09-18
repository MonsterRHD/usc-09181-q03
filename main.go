// Command observatory 启动跨境融资契约观察站服务。
//
// 环境变量：
//
//	PORT      监听端口（默认 8080）
//	DATA_FILE JSON 状态文件路径（默认 data/observatory.json），隔日重启据此恢复
package main

import (
	"log"
	"net/http"
	"os"

	"example.com/09181/q003/internal/api"
	"example.com/09181/q003/internal/service"
	"example.com/09181/q003/internal/store"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	dataFile := os.Getenv("DATA_FILE")
	if dataFile == "" {
		dataFile = "data/observatory.json"
	}

	svc := service.New(store.NewFileStore(dataFile))
	srv := api.NewServer(svc)

	log.Printf("跨境融资契约观察站已启动，监听 :%s，状态文件 %s", port, dataFile)
	if err := http.ListenAndServe(":"+port, srv.Handler()); err != nil {
		log.Fatalf("服务退出: %v", err)
	}
}
