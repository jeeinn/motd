// Package archtest 把设计稿 §2.2 的依赖方向变成会失败的测试。
//
// 本包只有测试代码，不参与运行时行为；doc.go 仅用于让目录在 go build 下也是合法包。
// 依赖方向是架构约束，不是编码习惯，因此必须由测试守住，而不是写在 README 里等人自觉。
package archtest
