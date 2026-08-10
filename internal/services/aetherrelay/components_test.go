package aetherrelay

import (
	_ "aetherrelay/internal/initiators/routeregistry"
	_ "aetherrelay/internal/modules/application/adminapi"
	_ "aetherrelay/internal/modules/application/proxyapi"
	_ "aetherrelay/internal/modules/blocks/configruntime"
	_ "aetherrelay/internal/modules/blocks/metricsruntime"
	_ "aetherrelay/internal/modules/blocks/usageruntime"
)
