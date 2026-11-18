package main

import (
	"reflect"
	"testing"
)

// 演练时踩过：flag 包遇到首个位置参数就停止解析，
// `restore <file> --target <path>` 会把 --target 吞掉。这里固化归一化行为。
func TestNormalizeFlags(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"后置 target", []string{"b.db", "--target", "x.db"}, []string{"--target", "x.db", "b.db"}},
		{"前置 target", []string{"--target", "x.db", "b.db"}, []string{"--target", "x.db", "b.db"}},
		{"等号形式", []string{"b.db", "--target=x.db"}, []string{"--target=x.db", "b.db"}},
		{"无 flag", []string{"b.db"}, []string{"b.db"}},
	}
	for _, c := range cases {
		got := normalizeFlags(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: normalizeFlags(%v) = %v，期望 %v", c.name, c.in, got, c.want)
		}
	}
}

func TestRestoreRequiresBothArguments(t *testing.T) {
	if err := cmdRestore(t.Context(), []string{"b.db"}); err == nil {
		t.Error("缺少 --target 时应返回用法错误")
	}
	if err := cmdRestore(t.Context(), []string{"--target", "x.db"}); err == nil {
		t.Error("缺少备份文件时应返回用法错误")
	}
}
