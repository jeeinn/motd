package user

// AchievementCode 是成就的稳定标识，写入 achievements.code。
// P0 只做骨架，白名单在这里定义；插件只能申请白名单内的成就。
type AchievementCode string

const (
	AchievementFirstPost AchievementCode = "first_post" // 首次发帖
	AchievementNightOwl  AchievementCode = "night_owl"  // 深夜发帖
	AchievementFirstVote AchievementCode = "first_vote" // 首次被投票
	AchievementStreak7   AchievementCode = "streak_7"   // 连续签到 7 天（v0.6.0）
)

var validAchievementCodes = map[AchievementCode]bool{
	AchievementFirstPost: true,
	AchievementNightOwl:  true,
	AchievementFirstVote: true,
	AchievementStreak7:   true,
}

func (c AchievementCode) Valid() bool { return validAchievementCodes[c] }
