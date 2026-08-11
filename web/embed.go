package web

import _ "embed"

// AdminIndexHTML 是项目目录 web/admin 下的 Provider 管理页，构建时嵌入二进制。
//
//go:embed admin/index.html
var AdminIndexHTML []byte

// AdminSiteIcon is the browser and mobile home-screen icon for the Admin UI.
//
//go:embed admin/assets/aetherrelay.png
var AdminSiteIcon []byte
