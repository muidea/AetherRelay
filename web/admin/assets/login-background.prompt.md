# AetherRelay 登录背景

- 生成方式：imagegen 技能，内置 `image_gen`，2026-09-07。
- 文件：`login-background.webp`，1024 × 576，11,454 字节（约 11.2 KiB）。按用户要求采用浅色风格，以雾白、冰蓝底色搭配淡青与淡紫的网络中继节点，中央安静留白。
- 按用户要求降低分辨率及体积：生成图原始尺寸 1672 × 941，PNG 1,379,980 字节；使用 ImageMagick 等比例缩小、移除元数据，以质量 80 编码 WebP，体积减少约 99.2%。原始生成图保留在生成工具的产物目录，不随程序打包。
- 使用：仅登录页加载，同源嵌入资源；居中 cover 裁切，移动端保留中央低对比区域；白色表单卡片保持可读性。图片加载失败时使用 CSS 白色至浅蓝渐变兜底。

## 最终生成提示词

Edit this AetherRelay login background to a LIGHT THEME. Preserve the landscape composition, two side clusters of abstract relay-network nodes and gently curving fine data paths, with quiet central 40% negative space. Replace ALL dark navy background with luminous off-white and extremely pale ice blue; center should be almost white, edges only softly pale blue. Turn the glowing neon into delicate translucent cyan, soft sky blue and a tiny amount of pale lavender, low contrast, elegant light airy premium SaaS infrastructure aesthetic. Nodes resemble small frosted-glass hexagonal relay hubs connected by hairline filaments, with soft ambient shadows so details remain visible against white. No dark areas, no black backdrop, no saturated neon, no noisy texture. Keep center exceptionally clean for an existing white login form card. No text, logo, watermark or UI elements. This is finished background artwork only. Retain the original wide aspect ratio.
