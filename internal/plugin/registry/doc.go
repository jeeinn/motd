// Package registry 维护编译期注册的插件表（init() 注册，无动态加载）。
// 边界：插件只能注册命令、订阅事件、调用受限只读接口，禁止直连数据库。
package registry
