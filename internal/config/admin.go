package config

import (
	"fmt"
	"strings"
)

// 管理员白名单的格式与 application/auth.ValidFingerprint 同口径：
// SSH 服务算出的指纹形如 SHA256:<43 位 base64>，不接受 MD5 或其它写法。
// 配置层只做语法校验；真正的准入判定统一由 application/admin.CanAdmin 给出。
const (
	adminFingerprintPrefix   = "SHA256:"
	adminFingerprintBodySize = 43
)

// ParseAdminFingerprints 解析逗号分隔的公钥指纹白名单。
//
// 口径：空串 = 无人可管（向后兼容）；空白项忽略；重复项去重；
// 非法指纹**启动即失败**（fail-closed），避免一个拼写错误把管理员锁在门外
// 却静默运行。错误文案只含配置名与项序号，不回显任何指纹或凭据。
func ParseAdminFingerprints(raw string) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	for i, part := range strings.Split(raw, ",") {
		fp := strings.TrimSpace(part)
		if fp == "" {
			continue
		}
		if !validAdminFingerprint(fp) {
			return nil, fmt.Errorf("MOTD_ADMIN_FINGERPRINTS 第 %d 项格式非法（需为 SHA256 指纹）", i+1)
		}
		out[fp] = struct{}{}
	}
	return out, nil
}

func validAdminFingerprint(fp string) bool {
	if !strings.HasPrefix(fp, adminFingerprintPrefix) {
		return false
	}
	body := fp[len(adminFingerprintPrefix):]
	if len(body) != adminFingerprintBodySize {
		return false
	}
	for _, c := range body {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/':
		default:
			return false
		}
	}
	return true
}
