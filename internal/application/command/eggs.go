package command

// v0.5.1 / P3-M6：彩蛋内容源。全部为本地固定语料或纯字符串生成，
// 无网络、无数据库、无用户数据回显。输出经 terminal/safe 渲染（cmd 层负责），
// 这里只产出可信的固定文案（ADR-019 / ADR-026）。
//
// 注意：本文件的函数都不接受用户输入参与文案生成——彩蛋文案里没有任何
// 用户输入，参数只做合法性校验，绝不把参数拼进输出。

import (
	"fmt"
	"math/rand/v2"
)

// EggCommands 返回所有彩蛋命令元数据（Name 不含前导 /）。
// cmd 装配层据此合并进命令注册表；Usage/Summary 会进入 Help()。
//
// ADR-075：/fortune、/xkcd、/blame 三个命令及其语料已删除，
// 本表只剩 /cowsay、/sudo、/matrix。
func EggCommands() []Command {
	return []Command{
		{Name: "cowsay", Group: GroupEgg, Usage: "/cowsay", Summary: "ASCII 牛说一句话", MaxArgs: 0},
		{Name: "sudo", Group: GroupEgg, Usage: "/sudo", Summary: "尝试提权（注定失败）", MaxArgs: 0},
		// ADR-065：/karma 命令入口已删除，声望/等级并入 /whoami；KarmaReader 能力层保留（状态栏消费）。
		{Name: "matrix", Group: GroupEgg, Usage: "/matrix", Summary: "看一小段数字雨（按 Ctrl-C 可中断）", MaxArgs: 0},
	}
}

var cowSayings = []string{
	"今天也要好好写代码。",
	"我先撤了，bug 你来。",
	"住手！那行代码不能用！",
	"已读，不回，在改了。",
	"你确定这不是特性？",
}

const cowArt = "  ______\r\n < %s >\r\n  ------\r\n         \\   ^__^\r\n          \\  (oo)\\_______\r\n             (__)\\       )\\/\\\r\n                 ||----w |\r\n                 ||     ||\r\n"

// Cowsay 返回一头说固定短句的 ASCII 牛。
func Cowsay() string {
	return fmt.Sprintf(cowArt, cowSayings[rand.IntN(len(cowSayings))])
}

const sudoText = "sudo: 权限不足。本次提权尝试已谢绝，并礼貌地记进了审计日志。\r\n提示：真正的管理员靠公钥指纹白名单，不靠 sudo。"

// Sudo 返回固定提权失败文案。
func Sudo() string { return sudoText }

const farewellText = "再见。别忘了在 /dev/null 之外留点痕迹。\r\n"

// Farewell 返回退出时的彩蛋文案（低概率触发由 cmd 层决定）。
func Farewell() string { return farewellText }
