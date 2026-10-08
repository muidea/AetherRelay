package aetherrelay

import (
	_ "aetherrelay/internal/initiators/routeregistry"
	_ "aetherrelay/internal/modules/application/adminapi"
	_ "aetherrelay/internal/modules/application/chatgptaccountpool"
	_ "aetherrelay/internal/modules/application/chatgptimagetask"
	_ "aetherrelay/internal/modules/application/chatgpttemporarychat"
	_ "aetherrelay/internal/modules/application/proxyapi"
	_ "aetherrelay/internal/modules/blocks/chatgptimagestore"
	_ "aetherrelay/internal/modules/blocks/chatgptwebupstream"
	_ "aetherrelay/internal/modules/blocks/codexaccountpool"
	_ "aetherrelay/internal/modules/blocks/codexupstream"
	_ "aetherrelay/internal/modules/blocks/configruntime"
	_ "aetherrelay/internal/modules/blocks/metricsruntime"
	_ "aetherrelay/internal/modules/blocks/usageruntime"
)
