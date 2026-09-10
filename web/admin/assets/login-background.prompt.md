# AetherRelay 登录背景

- 生成方式：`imagegen` 技能，内置 `image_gen`，2026-09-10。
- 文件：`login-background.webp`，1600 × 900，WebP。采用雾白、冰蓝和淡青色的浅色科技风格，以半透明中继节点、数据环和流线表达 API 路由网络。
- 构图：主要视觉元素分布于左右两侧，中央约 42% 保持明亮、低对比留白，用于承载现有白色登录卡片；宽屏与移动端 `cover` 裁切时仍能保持表单可读性。
- 约束：无文字、Logo、水印和 UI 控件，不含深色区域；图片加载失败时继续使用登录页 CSS 渐变兜底。

## 最终生成提示词

```text
Use case: ui-mockup
Asset type: production login page background artwork for AetherRelay, wide landscape 16:9
Primary request: Create a completely new light-theme technology background that suggests intelligent API routing, relay networks, and flowing data.
Scene/backdrop: luminous pearl-white and extremely pale ice-blue atmospheric field, clean and airy.
Subject: abstract translucent glass relay nodes and layered circular data rings distributed mainly along the far left and far right edges, connected by graceful thin cyan data paths and a few softly glowing particles; subtle depth from frosted glass, soft shadows, and restrained blur.
Style/medium: premium contemporary SaaS infrastructure illustration, polished 3D glassmorphism blended with precise technical linework, elegant and calm.
Composition/framing: wide landscape; keep the central 42% exceptionally quiet, bright, uncluttered, and low contrast for a centered white login form card; visual interest should frame the card from both sides and remain useful when cover-cropped on desktop and mobile.
Lighting/mood: high-key diffused daylight, trustworthy, calm, advanced.
Color palette: off-white, ice blue, pale cyan, soft sky blue, with only a tiny amount of pale lavender; restrained saturation.
Constraints: finished background artwork only; raster image; all technical elements must remain decorative and abstract; strong readability behind a white card; no text, no letters, no numbers, no logo, no watermark, no UI controls, no dark regions.
Avoid: cyberpunk, dark navy, black, saturated neon, dense circuit-board patterns, busy center, stock-photo look, hard contrast, noisy texture.
```
