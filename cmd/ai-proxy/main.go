package main

import (
	"os"

	_ "ai-proxy/internal/initiators/routeregistry"
	_ "ai-proxy/internal/modules/application/adminapi"
	_ "ai-proxy/internal/modules/application/chatgptaccountpool"
	_ "ai-proxy/internal/modules/application/chatgptimagetask"
	_ "ai-proxy/internal/modules/application/chatgpttemporarychat"
	_ "ai-proxy/internal/modules/application/proxyapi"
	_ "ai-proxy/internal/modules/blocks/chatgptimagestore"
	_ "ai-proxy/internal/modules/blocks/chatgptwebupstream"
	_ "ai-proxy/internal/modules/blocks/configruntime"
	_ "ai-proxy/internal/modules/blocks/metricsruntime"
	_ "ai-proxy/internal/modules/blocks/usageruntime"
	"ai-proxy/internal/services/aiproxy"
)

// version 由 scripts/build-release.sh 通过 -ldflags 注入；本地开发保持 dev。
var version = "dev"

func main() { os.Exit(aiproxy.Run(version)) }
