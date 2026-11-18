// Package safe 实现终端侧的分层安全渲染（评审稿 §3.5）。
//
// 三段严格分离，任何一段都不能被跳过：
//  1. SanitizeText —— 纯文本净化：控制序列可见化 / 剥离、CR 与 Tab 归一、
//     双向文本控制符剥离、超长行折行；
//  2. ParseInline —— 白名单 Markdown 解析，只识别 4 种语法，链接协议白名单；
//  3. renderInline —— 受控 ANSI 生成，输出中的 ESC 序列**只能**来自 ansi.go
//     的 SGR 常量表，禁止光标定位 / 清屏 / 窗口标题 / 剪贴板等一切其它序列。
//
// 服务端可信文本（状态栏、提示）走 RenderSystem，用户提交的内容走 RenderUser，
// 两者不共用同一个 sanitize；由 archtest 静态守卫强制，不允许偷偷合并。
//
// 边界：本包只 import 标准库，不依赖 transport / infrastructure / plugin / cmd，
// 也不含任何终端尺寸或会话状态（那些由调用方以 Options.Width 传入）。
package safe
