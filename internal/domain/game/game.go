// Package game 是 /game 的纯领域状态机（R-14 / ADR-078）。
//
// 边界：只管棋盘状态、按键规则、计分与有效局判定；产出纯数据 Frame
// （字符 + 色阶）。**不含任何 ESC、不碰 IO、不 import 上层包**——与
// application/command/matrix.go 同构（ADR-027 信任域与分层约束）。
// 色阶由 presentation/terminal 层映射到基础 16 色 SGR。
//
// V1.0 只有 2048 与 Snake 两款（ADR-078 决策 2）；扫雷 V1.1 才加入，
// 届时只需扩展 GameKey 与 NewSession 的白名单，DB 不需迁移（game_key 无 CHECK 枚举）。
package game

import "errors"

var (
	ErrUnknownGame  = errors.New("未知游戏")
	ErrInvalidMove  = errors.New("无效移动")
	ErrSessionOver  = errors.New("本局已结束")
	ErrInvalidInput = errors.New("无效输入")
)

// GameKey 是服务端白名单游戏标识。DB 的 game_scores.game_key 存此值；
// 它只能是本包定义的常量，绝不接收用户原始输入（ADR-026 零回显）。
type GameKey string

const (
	Key2048  GameKey = "2048"
	KeySnake GameKey = "snake"
)

// Valid 判定是否为 V1.0 已实现的游戏白名单。
func (k GameKey) Valid() bool {
	return k == Key2048 || k == KeySnake
}

// Ink 是格子的色阶枚举。渲染层据此选 SGR，本包不产 ESC。
// 基础 16 色内的语义档，不含 256 色/真彩（ROADMAP §4 冻结）。
type Ink uint8

const (
	InkPlain  Ink = iota // 空格 / 普通格（默认色）
	InkBoard             // 棋盘边框与底纹（暗色）
	InkValue             // 数字格（普通数字）
	InkMerged            // 本步合并产生的数字（高亮）
	InkHead              // 蛇头
	InkBody              // 蛇身
	InkFood              // 食物
)

// Cell 是一个棋盘格：文本 + 色阶。文本恒为可打印 ASCII（ADR-076 字形坑），
// 且由领域层保证固定列宽——2048 每格 4 列右对齐（"   2".."2048"），
// Snake 每格 1 列。渲染层直接按行拼接，不在 presentation 层做宽度计算。
type Cell struct {
	Text string
	Ink  Ink
}

// Frame 是一帧纯数据：行矩阵 + 计分 + 结束态 + 本局有效局判定。
// presentation 层把它组装成整屏帧；本包不知道终端尺寸与 ANSI。
type Frame struct {
	Rows  [][]Cell
	Score int
	Over  bool
	Valid bool   // 有效局门槛（ADR-078 参数）：2048 有效移动≥5 且得分≥4；Snake 食物≥1
	Hint  string // 固定提示文案（服务端常量，非用户输入）
}

// Direction 是输入驱动的方向（方向键/WASD 归一化后的领域值）。
type Direction uint8

const (
	DirUp Direction = iota
	DirDown
	DirLeft
	DirRight
)

// Session 是一款游戏的单局会话状态机。每会话一份，由 cmd 层持有。
// 内部字段不导出：外部只能通过 Step / Frame 观察与驱动。
type Session struct {
	key   GameKey
	board *board2048
	snake *snakeState
	steps int // 有效移动步数
	score int
	over  bool
}

// NewSession 按白名单创建一局。未知游戏返回 ErrUnknownGame（调用方据此给固定提示，
// 绝不回显用户输入的 name，ADR-026）。
func NewSession(key GameKey) (*Session, error) {
	switch key {
	case Key2048:
		return &Session{key: key, board: newBoard2048()}, nil
	case KeySnake:
		return &Session{key: key, snake: newSnake()}, nil
	default:
		return nil, ErrUnknownGame
	}
}

// Key 返回本局的游戏标识。
func (s *Session) Key() GameKey { return s.key }

// Step 应用一次方向输入，返回本帧。结束态后再输入返回 ErrSessionOver。
// 每帧输出由渲染层整屏重绘（RenderCleanFrame），scrollback 零增长。
func (s *Session) Step(d Direction) (Frame, error) {
	if s.over {
		return Frame{}, ErrSessionOver
	}
	var moved bool
	switch s.key {
	case Key2048:
		score, ok := s.board.move(d)
		moved = ok
		if ok {
			s.score += score
			s.steps++
			if s.board.full() && !s.board.hasMoves() {
				s.over = true
			}
		}
	case KeySnake:
		ate, dead := s.snake.step(d)
		if dead {
			s.over = true
			moved = true
		} else if ate {
			s.score += 10
			s.steps++
			moved = true
		} else {
			// 转向本身也算一步有效操作（蛇每步都在移动）。
			moved = true
			s.steps++
		}
	}
	if !moved {
		return Frame{}, ErrInvalidMove
	}
	return s.frame(), nil
}

// Frame 产出当前帧的纯数据。
func (s *Session) Frame() Frame { return s.frame() }

func (s *Session) frame() Frame {
	var rows [][]Cell
	hint := "方向键/WASD 移动 · q 退出"
	switch s.key {
	case Key2048:
		rows = s.board.cells()
		if s.over {
			hint = "无路可走 · 按 q 退出"
		}
	case KeySnake:
		rows = s.snake.cells()
		if s.over {
			hint = "撞墙 · 按 q 退出"
		}
	}
	// 有效局门槛（ADR-078 参数，观察期可校准）。
	valid := false
	switch s.key {
	case Key2048:
		valid = s.steps >= 5 && s.score >= 4
	case KeySnake:
		valid = s.score >= 10 // 至少吃到 1 个食物
	}
	return Frame{Rows: rows, Score: s.score, Over: s.over, Valid: valid, Hint: hint}
}
