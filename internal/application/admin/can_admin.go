// Package admin 是节点治理权限的唯一判定点。
//
// 权限模型（ADR-049 / P2-M1）：只认服务端认证阶段算出的 SSH 公钥指纹白名单，
// 不建 users.role、不做 RBAC、不接受用户提交文本。本包只导出一个谓词，
// 所有 /node 治理用例都经它放行，archtest 静态守卫这条单点约束。
package admin

import "github.com/jeeinn/motd/internal/application/auth"

// CanAdmin 报告一个已认证会话指纹是否在白名单内。
//
// 调用方传入的 fingerprint 必须来自 Principal.KeyFingerprint（服务端持有），
// 绝不可以是命令参数。空指纹、格式非法、空白名单一律拒绝（fail-closed）。
// 指纹大小写敏感：与配置里的字符串逐字节比对，不做归一化。
func CanAdmin(fingerprint string, whitelist map[string]struct{}) bool {
	if fingerprint == "" || len(whitelist) == 0 {
		return false
	}
	if !auth.ValidFingerprint(fingerprint) {
		return false
	}
	_, ok := whitelist[fingerprint]
	return ok
}
