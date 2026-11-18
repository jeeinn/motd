// Package terminal 是终端侧 Renderer：把 ViewModel 渲染为受控 ANSI。
// 边界：只能通过白名单解析器生成 ANSI；用户文本与系统样式分属不同信任域（§3.5）。
package terminal
