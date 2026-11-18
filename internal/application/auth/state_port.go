package auth

import "context"

// 状态机端口（评审稿 §2.2 / ADR-001「端口定义在使用方」、ADR-010 UoW）。
//
// 与 forum.Repos 同构：application 只声明接口，实现落在 infrastructure/sqlite；
// 读走只读门面，写一律经 UnitOfWork.Write 拿到受限 WriteTx，拿不到 *sql.Tx。
// 网络端点（DeviceFlowClient，P0-15）与持久化端口在这里彻底解耦：
// 状态机用例同时持有两者，但不把任何一方塞进另一方。

// DeviceAuthReader 是设备码状态的只读端口，走读池（不占写连接）。
type DeviceAuthReader interface {
	// GetByDeviceCode 按 device_code 精确查一行；不存在返回 ErrDeviceAuthNotFound。
	GetByDeviceCode(ctx context.Context, deviceCode string) (DeviceAuthState, error)
}

// DeviceAuthWriteTx 是单个写事务内的受限句柄。
//
// 所有变更方法都带 CAS 语义：UPDATE 的 WHERE 里同时钉住 id 与期望的旧状态，
// RowsAffected == 0 时返回 ErrStateConflict。理由：轮询的「读状态」发生在
// 写事务之外（网络调用不能占着唯一写连接），期间可能被取消 / 重复发起改动，
// 若不做条件更新，迟到的轮询结果会把终态覆盖回 pending 或 authorized。
type DeviceAuthWriteTx interface {
	// Insert 新建一条 pending 记录，返回自增 ID。
	Insert(ctx context.Context, s NewDeviceAuthState) (int64, error)

	// SupersedeSessionPending 把同一 session_ref 下所有 pending 记录批量迁移到 to
	// （同会话重复发起，以最后一次为准并作废旧码）。返回作废条数。
	// 只允许 to 为终态，且必须在事务内与 Insert 同一个原子单元完成。
	SupersedeSessionPending(ctx context.Context, sessionRef string, to DeviceAuthStatus) (int64, error)

	// Transition 做 CAS 状态迁移：WHERE id = ? AND status = expectFrom。
	// 调用前应已用 CanTransition 校验过路径；这里再兜一层实现侧的防线。
	Transition(ctx context.Context, id int64, expectFrom, to DeviceAuthStatus) error

	// UpdateInterval 在保持 expectFrom（通常 pending）的前提下更新轮询间隔
	// （slow_down 后的 NextInterval 结果持久化）。同样带 CAS。
	UpdateInterval(ctx context.Context, id int64, expectFrom DeviceAuthStatus, interval int) error
}

// DeviceAuthUnitOfWork 把「一次状态变更 = 一个事务」变成结构性约束。
type DeviceAuthUnitOfWork interface {
	Write(ctx context.Context, fn func(DeviceAuthWriteTx) error) error
}

// DeviceAuthStore 是装配时注入给状态机用例的完整端口集合。
type DeviceAuthStore interface {
	DeviceAuthReader
	DeviceAuthUnitOfWork
}
