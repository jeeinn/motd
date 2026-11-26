package game

import (
	"testing"
)

func TestGameKeyWhitelist(t *testing.T) {
	if !Key2048.Valid() || !KeySnake.Valid() {
		t.Fatal("V1.0 两款游戏应在白名单内")
	}
	if GameKey("minesweeper").Valid() {
		t.Fatal("扫雷属 V1.1，不应在 V1.0 白名单")
	}
	if GameKey("../../etc/passwd").Valid() {
		t.Fatal("非法游戏名不得被接受")
	}
	if _, err := NewSession(GameKey("bogus")); err != ErrUnknownGame {
		t.Fatalf("未知游戏应返回 ErrUnknownGame，实际 %v", err)
	}
}

func TestMergeLine(t *testing.T) {
	cases := []struct {
		in    [boardSize]int
		want  [boardSize]int
		score int
	}{
		{[4]int{2, 2, 0, 0}, [4]int{4, 0, 0, 0}, 4},
		{[4]int{2, 2, 2, 2}, [4]int{4, 4, 0, 0}, 8}, // 每格每步只合并一次
		{[4]int{4, 4, 8, 8}, [4]int{8, 16, 0, 0}, 24},
		{[4]int{2, 0, 2, 4}, [4]int{4, 4, 0, 0}, 4},
		{[4]int{2, 4, 8, 16}, [4]int{2, 4, 8, 16}, 0},
		{[4]int{0, 0, 0, 0}, [4]int{0, 0, 0, 0}, 0},
	}
	for _, c := range cases {
		got, score := mergeLine(c.in)
		if got != c.want || score != c.score {
			t.Errorf("mergeLine(%v) = %v, %d; want %v, %d", c.in, got, score, c.want, c.score)
		}
	}
}

func Test2048MoveChangesAndScores(t *testing.T) {
	s, err := NewSession(Key2048)
	if err != nil {
		t.Fatal(err)
	}
	// 直接设置棋盘（领域内部测试），构造一次确定合并。
	s.board.grid = [boardSize][boardSize]int{
		{2, 2, 0, 0},
		{0, 0, 0, 0},
		{0, 0, 0, 0},
		{0, 0, 0, 0},
	}
	f, err := s.Step(DirLeft)
	if err != nil {
		t.Fatal(err)
	}
	if f.Score != 4 {
		t.Fatalf("合并 2+2 应得 4 分，实际 %d", f.Score)
	}
	if len(f.Rows) != boardSize || len(f.Rows[0]) != boardSize {
		t.Fatalf("帧尺寸不符: %dx%d", len(f.Rows), len(f.Rows[0]))
	}
	if f.Rows[0][0].Text != "   4" {
		t.Fatalf("合并格应显示 4 列右对齐的 '   4'，实际 %q", f.Rows[0][0].Text)
	}
}

func Test2048ValidThreshold(t *testing.T) {
	s, _ := NewSession(Key2048)
	s.board.grid = [boardSize][boardSize]int{}
	s.steps, s.score = 4, 4
	if s.Frame().Valid {
		t.Fatal("有效移动<5 步不应算有效局")
	}
	s.steps = 5
	if !s.Frame().Valid {
		t.Fatal("≥5 步且得分≥4 应算有效局")
	}
}

func Test2048MoveReturnsErrOnNoChange(t *testing.T) {
	// 一行 [2,4,8,16] 向左合并无变化：mergeLine 应返回 unchanged。
	in := [boardSize]int{2, 4, 8, 16}
	out, score := mergeLine(in)
	if out != in || score != 0 {
		t.Fatalf("无可合并行应原样返回: got %v score=%d", out, score)
	}
}

func TestSnakeOppositeIgnored(t *testing.T) {
	s, err := NewSession(KeySnake)
	if err != nil {
		t.Fatal(err)
	}
	startHead := s.snake.body[0]
	// 初始朝右，输入左（180°）应被忽略，仍向右走。
	_, dead := s.snake.step(DirLeft)
	if dead {
		t.Fatal("180° 转向不应立即判死")
	}
	if s.snake.body[0].x != startHead.x+1 {
		t.Fatalf("反向输入应被忽略继续向右，head=%+v", s.snake.body[0])
	}
}

func TestSnakeWallDeath(t *testing.T) {
	s, _ := NewSession(KeySnake)
	// 把蛇头放到右边界、朝右，下一步撞墙。通过 Session.Step 驱动以同步 over 态。
	s.snake.body = []point{{x: snakeWidth - 1, y: 5}}
	s.snake.dir = DirRight
	f, err := s.Step(DirRight)
	if err != nil {
		t.Fatalf("撞墙这一步本身应正常返回帧: %v", err)
	}
	if !f.Over {
		t.Fatal("撞墙后帧应为结束态")
	}
	if _, err := s.Step(DirUp); err != ErrSessionOver {
		t.Fatalf("结束后再输入应返回 ErrSessionOver，实际 %v", err)
	}
}

func TestSnakeValidThreshold(t *testing.T) {
	s, _ := NewSession(KeySnake)
	if s.Frame().Valid {
		t.Fatal("未吃食物不应算有效局")
	}
	// 把食物放到蛇头右侧，吃一个。
	s.snake.body = []point{{x: 5, y: 5}, {x: 4, y: 5}}
	s.snake.dir = DirRight
	s.snake.food = point{x: 6, y: 5}
	ate, dead := s.snake.step(DirRight)
	if !ate || dead {
		t.Fatalf("应吃到食物，实际 ate=%v dead=%v", ate, dead)
	}
	s.score += 10
	if !s.Frame().Valid {
		t.Fatal("吃到≥1 食物应算有效局")
	}
}

func TestFrameUsesASCIIOnly(t *testing.T) {
	for _, key := range []GameKey{Key2048, KeySnake} {
		s, _ := NewSession(key)
		f := s.Frame()
		for _, row := range f.Rows {
			for _, cell := range row {
				for _, r := range cell.Text {
					if r < 0x20 || r > 0x7e {
						t.Fatalf("%s 棋盘含非 ASCII 可打印字符: %q", key, cell.Text)
					}
				}
			}
		}
	}
}
