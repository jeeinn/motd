package viewmodel

import (
	"reflect"
	"testing"
	"time"
)

// P0-11：状态模型只含语义值。用反射把「允许类型」固化下来，
// 避免后来有人为了方便塞进函数、指针、接口或 time.Time 等渲染/运行时概念。

func semanticTypeErrors(typ reflect.Type, seen map[reflect.Type]bool) []string {
	if seen[typ] {
		return nil
	}
	seen[typ] = true
	switch typ.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int64, reflect.Uint8, reflect.String:
		return nil
	case reflect.Slice:
		return semanticTypeErrors(typ.Elem(), seen)
	case reflect.Struct:
		wantPkg := reflect.TypeOf(Screen{}).PkgPath()
		var out []string
		if typ.PkgPath() != wantPkg {
			out = append(out, "外部/匿名结构体类型不允许: "+typ.String())
		}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			errs := semanticTypeErrors(f.Type, seen)
			for _, e := range errs {
				out = append(out, f.Name+": "+e)
			}
		}
		return out
	default:
		return []string{"非语义类型 kind=" + typ.Kind().String() + " (" + typ.String() + ")"}
	}
}

func TestScreenTypesAreSemanticOnly(t *testing.T) {
	if errs := semanticTypeErrors(reflect.TypeOf(Screen{}), map[reflect.Type]bool{}); len(errs) > 0 {
		t.Fatalf("Screen 含非语义字段:\n  %v", errs)
	}
}

// TestSemanticGuardDetectsBadTypes 证明上面那条守卫本身会失败，而非恒真。
func TestSemanticGuardDetectsBadTypes(t *testing.T) {
	bad := []struct {
		name string
		typ  reflect.Type
	}{
		{"函数", reflect.TypeOf(struct{ F func() }{})},
		{"指针", reflect.TypeOf(struct{ P *int }{})},
		{"接口", reflect.TypeOf(struct{ A any }{})},
		{"外部时间类型", reflect.TypeOf(struct{ T time.Time }{})},
		{"通道", reflect.TypeOf(struct{ C chan int }{})},
		{"映射", reflect.TypeOf(struct{ M map[string]int }{})},
	}
	for _, c := range bad {
		if errs := semanticTypeErrors(c.typ, map[reflect.Type]bool{}); len(errs) == 0 {
			t.Errorf("注入%s字段后守卫未报错，规则失效", c.name)
		}
	}
}

func TestScreenKindValid(t *testing.T) {
	if ScreenUnknown.Valid() {
		t.Error("零值页面种类不应合法：调用方必须显式指定")
	}
	for _, k := range []ScreenKind{ScreenNodes, ScreenThreads, ScreenThreadDetail, ScreenComposer, ScreenLogin, ScreenHelp} {
		if !k.Valid() {
			t.Errorf("页面种类 %d 应合法", k)
		}
	}
	if (ScreenKind(200)).Valid() || (ScreenKind(7)).Valid() {
		t.Error("越界页面种类不应合法")
	}
	if !NoticeNone.Valid() || (NoticeKind(5)).Valid() {
		t.Error("提示种类取值域不正确")
	}
	if !OverlayNone.Valid() || !OverlayDeviceCode.Valid() || (OverlayKind(5)).Valid() {
		t.Error("浮层种类取值域不正确")
	}
	if !DeviceFlowIdle.Valid() || (DeviceFlowState(6)).Valid() {
		t.Error("登录阶段取值域不正确")
	}
}

func validScreen() Screen {
	return Screen{
		Kind:      ScreenNodes,
		StatusBar: StatusBar{Level: 1},
		Rows: []Row{
			{Index: 1, ID: 10},
			{Index: 2, ID: 11},
		},
		Selection: Selection{FocusedIndex: 1},
	}
}

func TestScreenValidate(t *testing.T) {
	if err := validScreen().Validate(); err != nil {
		t.Fatalf("合法屏幕被拒绝: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*Screen)
	}{
		{"未指定页面种类", func(s *Screen) { s.Kind = ScreenUnknown }},
		{"等级未设置", func(s *Screen) { s.StatusBar.Level = 0 }},
		{"等级越界", func(s *Screen) { s.StatusBar.Level = 5 }},
		{"提示种类越界", func(s *Screen) { s.StatusBar.Notice.Kind = 99 }},
		{"浮层种类越界", func(s *Screen) { s.Overlay.Kind = 99 }},
		{"登录浮层阶段越界", func(s *Screen) {
			s.Overlay.Kind = OverlayDeviceCode
			s.Overlay.Progress.State = 99
		}},
		{"行序号不递增", func(s *Screen) { s.Rows = []Row{{Index: 1}, {Index: 1}} }},
		{"行序号为零", func(s *Screen) { s.Rows = []Row{{Index: 0}} }},
		{"焦点越界", func(s *Screen) { s.Selection.FocusedIndex = 99 }},
		{"焦点为负", func(s *Screen) { s.Selection.FocusedIndex = -1 }},
		{"分页序号为负", func(s *Screen) { s.Pagination.FirstIndex = -1 }},
	}
	for _, c := range cases {
		s := validScreen()
		c.mutate(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s 应被拒绝", c.name)
		}
	}
}

// 空列表是合法状态（例如空版块、无回复），焦点为 0 不应触发越界。
func TestScreenValidateEmptyRows(t *testing.T) {
	s := validScreen()
	s.Rows = nil
	s.Selection = Selection{}
	if err := s.Validate(); err != nil {
		t.Fatalf("空列表屏应合法: %v", err)
	}
}
