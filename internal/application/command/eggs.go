package command

// v0.5.1 / P3-M6：彩蛋内容源。全部为本地固定语料或纯字符串生成，
// 无网络、无数据库、无用户数据回显。输出经 terminal/safe 渲染（cmd 层负责），
// 这里只产出可信的固定文案（ADR-019 / ADR-026）。
//
// 注意：本文件的函数都不接受用户输入参与文案生成——/blame 等带参命令
// 在 cmd 层只做「参数合法即回固定文案」，绝不把参数拼进输出。

import (
	"fmt"
	"math/rand/v2"
)

// EggCommands 返回所有彩蛋命令元数据（Name 不含前导 /）。
// cmd 装配层据此合并进命令注册表；Usage/Summary 会进入 Help()。
func EggCommands() []Command {
	return []Command{
		{Name: "fortune", Usage: "/fortune", Summary: "随机吐一句箴言", MaxArgs: 0},
		{Name: "cowsay", Usage: "/cowsay", Summary: "ASCII 牛说一句话", MaxArgs: 0},
		{Name: "xkcd", Usage: "/xkcd", Summary: "随机一则 xkcd 风格标题（本地清单）", MaxArgs: 0},
		{Name: "blame", Usage: "/blame @user", Summary: "把锅甩给某个倒霉蛋（玩笑）", MinArgs: 1, MaxArgs: 1},
		{Name: "sudo", Usage: "/sudo", Summary: "尝试提权（注定失败）", MaxArgs: 0},
		{Name: "karma", Usage: "/karma", Summary: "查看自己的声望与等级", MaxArgs: 0},
	}
}

var fortunes = []string{
	"症结不在键盘，在椅子与键盘之间那 60 厘米。",
	"能跑的代码别重构，能睡的觉别熬夜。",
	"每一个 bug 都是未来维护者给你的来信。",
	"过早优化是万恶之源，过晚优化是失业之源。",
	"世界上只有两种语言：被人抱怨的和没人用的。",
	"缓存失效与信任危机，都是最难排查的两类问题。",
	"删掉的代码才是最优质的代码。",
	"注释和诺言一样，过期了比没有更危险。",
	"测试通过只说明没发现 bug，不说明没有。",
	"最好的错误处理，是不产生错误的那条路径。",
	"上线前的最后一次改动，是事故最常见的种子。",
	"你以为在调程序，其实是程序在调你的耐心。",
	"重构的第一定律：改之前先有测试。第二定律：先跑一遍。",
	"命令行不会骗你，除非你看错了输出。",
	"慢查询不可怕，可怕的是它偶尔快。",
	"技术债不还不删，它只会利滚利。",
	"所谓经验，就是把踩过的坑写成文档然后忘了看。",
	"能用 sed 解决的，就不要写服务。",
	"并发的悖论：越想同时做，越要排队做。",
	"最好的彩蛋，是让人愿意多按一次回车。",
}

// Fortune 返回一句随机箴言（固定语料，无网络）。
func Fortune() string {
	return "箴言：" + fortunes[rand.IntN(len(fortunes))]
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

var xkcdTitles = []string{
	"353 — Approval",
	"378 — Duty Calls",
	"427 — Django",
	"519 — sudo",
	"563 — The Dinosaur's Question",
	"665 — Who's There?",
	"803 — parody",
	"974 — Extracting Values",
	"1174 — Python",
	"1567 — We Did It!",
	"1987 — Questions",
	"2037 — Terminal Velocity",
}

// XKCD 返回一条本地清单中的 xkcd 标题（不联网）。
func XKCD() string {
	return "xkcd " + xkcdTitles[rand.IntN(len(xkcdTitles))]
}

var blameLines = []string{
	"经查证，锅在那个没写测试的人头上。",
	"这不是你的问题，是宇宙的问题。生成器已崩溃。",
	"已联系相关负责人，对方表示「在改了」。",
	"锅已成功转移，当前会话心理压力下降 12%。",
	" blame 报告：责任链追溯至 3 次提交前，线索中断。",
}

// Blame 返回固定甩锅文案。参数只用于「是否给了参数」的判断，绝不回显。
func Blame() string {
	return blameLines[rand.IntN(len(blameLines))]
}

const sudoText = "sudo: 权限不足。本次提权尝试已谢绝，并礼貌地记进了审计日志。\r\n提示：真正的管理员靠公钥指纹白名单，不靠 sudo。"

// Sudo 返回固定提权失败文案。
func Sudo() string { return sudoText }

const farewellText = "再见。别忘了在 /dev/null 之外留点痕迹。\r\n"

// Farewell 返回退出时的彩蛋文案（低概率触发由 cmd 层决定）。
func Farewell() string { return farewellText }
