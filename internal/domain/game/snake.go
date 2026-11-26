package game

import "math/rand/v2"

// Snake 棋盘尺寸：20 列 × 10 行，纯 ASCII，适配 80×24 常见终端。
const (
	snakeWidth  = 20
	snakeHeight = 10
)

type point struct{ x, y int }

type snakeState struct {
	body []point // [0] 是蛇头
	dir  Direction
	food point
	dead bool
	ate  int
}

func newSnake() *snakeState {
	s := &snakeState{
		body: []point{{x: 5, y: 5}, {x: 4, y: 5}, {x: 3, y: 5}},
		dir:  DirRight,
	}
	s.placeFood()
	return s
}

// placeFood 在空格随机放食物。满盘时不再放（理论极端情况）。
func (s *snakeState) placeFood() {
	for attempt := 0; attempt < 100; attempt++ {
		p := point{x: rand.IntN(snakeWidth), y: rand.IntN(snakeHeight)}
		if !s.occupies(p) {
			s.food = p
			return
		}
	}
}

func (s *snakeState) occupies(p point) bool {
	for _, b := range s.body {
		if b == p {
			return true
		}
	}
	return false
}

// step 应用一次方向输入。返回 (是否吃到食物, 是否死亡)。
// 180° 反向输入被忽略（按当前方向前进），不判死——经典 Snake 常见宽限，避免手滑秒死。
func (s *snakeState) step(d Direction) (bool, bool) {
	if s.dead {
		return false, true
	}
	if !opposite(d, s.dir) {
		s.dir = d
	}
	head := s.body[0]
	next := head
	switch s.dir {
	case DirUp:
		next.y--
	case DirDown:
		next.y++
	case DirLeft:
		next.x--
	case DirRight:
		next.x++
	}
	// 撞墙判定。
	if next.x < 0 || next.x >= snakeWidth || next.y < 0 || next.y >= snakeHeight {
		s.dead = true
		return false, true
	}
	// 撞自身判定（尾巴本步会移动，故允许移动到原尾格）。
	growing := next == s.food
	limit := len(s.body)
	if !growing {
		limit--
	}
	for i := 0; i < limit; i++ {
		if s.body[i] == next {
			s.dead = true
			return false, true
		}
	}
	s.body = append([]point{next}, s.body...)
	if growing {
		s.ate++
		s.placeFood()
		return true, false
	}
	s.body = s.body[:len(s.body)-1]
	return false, false
}

func opposite(a, b Direction) bool {
	return (a == DirUp && b == DirDown) || (a == DirDown && b == DirUp) ||
		(a == DirLeft && b == DirRight) || (a == DirRight && b == DirLeft)
}

// cells 产出纯数据帧：蛇头/蛇身/食物三档色阶，其余为棋盘底纹。
func (s *snakeState) cells() [][]Cell {
	rows := make([][]Cell, snakeHeight)
	for y := 0; y < snakeHeight; y++ {
		rows[y] = make([]Cell, snakeWidth)
		for x := 0; x < snakeWidth; x++ {
			rows[y][x] = Cell{Text: ".", Ink: InkBoard}
		}
	}
	for i, p := range s.body {
		ink := InkBody
		text := "o"
		if i == 0 {
			ink = InkHead
			text = "@"
		}
		rows[p.y][p.x] = Cell{Text: text, Ink: ink}
	}
	rows[s.food.y][s.food.x] = Cell{Text: "*", Ink: InkFood}
	return rows
}

// ForceStateForTest 仅供跨包测试构造必死局面（蛇头贴右墙、朝右），
// 生产代码永不调用。测试通过它避免依赖随机的蛇移动序列。
func (s *Session) ForceStateForTest() {
	if s.snake != nil {
		s.snake.body = []point{{x: snakeWidth - 1, y: 5}}
		s.snake.dir = DirRight
		s.snake.food = point{x: 0, y: 0}
	}
}
