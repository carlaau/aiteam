// status.go —— status overview 聚合 store（B3-3/B3-5）：§2.3 响应结构逐字段装配 +
// §3.7 往返基线（基线 9 条 + B3-5 接线窗聚合两查与 progress 一查 + b3-W1 会话未读
// 聚合一查共 13 条，不随项目/栏目数增长——机械闸 TestRoundtripBudget）。
//
// 分层口径（信箱#11）→ B3-8 收敛裁定：poll 可见性谓词/位点读取/block 未回执清单
// 三处在 B2 未合并期按 §3.6 冻结 SQL 自含实现（不触碰 messages.go/positions.go/
// receipts.go，防合并冲突）；B2 关批 rebase 后经 B3-8 逐项对审，四处（poll 谓词项
// 按 direct/bus 两函数落地）均**保留自含
// 实现**——B2 导出面（PollVisible/GetPositions/ListUnreceipted）为单信箱/单发送方
// 行级形态，与 status 的全局 GROUP BY 聚合纯读形态不匹配，改调 B2 函数即 N+1 或
// 引入写副作用，破坏 §3.7 往返基线与读路径零写；谓词文本与 §3.6/B2 同源一致
// （b3-spec §六「SQL 文本一致时允许保留但须注明」），各查询函数注释留处置理由，
// 不留静默双路径。windows 段（§2.3 columns[].windows）消费 B4 产物 ListWindowStatus
// 压缩装配（装配体在 status_windows_progress.go，禁自含复刻）；progress 段（FR23
// 增补）消费 B8 产物 LatestProgressBySession，同文件装配。
//
// 时钟纪律：now 全参数注入（RFC3339 带时区形态），SQL 内零 'now' 字面量（§3.6 失联 SQL
// 的 strftime('%s','now') 改绑定参数——与 B1 失联计算/B3-1 哨兵活性同口径）。
package store

import (
	"fmt"
	"slices"
	"strings"
)

// 默认失联/哨兵阈值（§7.2 session.heartbeat_timeout_sec=900；§7.3 sentinel_timeout=15）。
// opts 同名字段零值时生效；服务配置经 handler 层透传覆盖。
// DefaultSentinelTimeoutSec 导出：#14 B3-T4 机械闸的兜底口径（server 包引用，审查 K1
// 修复——哨兵域单一阈值来源，勿另立常量）。
const (
	defaultSessionTimeoutSec  int64 = 900
	DefaultSentinelTimeoutSec int64 = 15
)

// chatPositionPrefix 会话对话位点的 consumer 前缀（§3.2 表 7）：信箱口径只认角色位点，
// chat: 前缀位点归会话对话流，不进 mailboxes 角色集。
const chatPositionPrefix = "chat:"

// OverviewConfig §2.3 config 回显段（B5-6 增补，b5-spec §八「看板配置展示区」
// 数据源，总控 #14②）：服务侧三阈值生效值回显——BuildOverview 收到的 opts 三阈值
// 经零值兜底后的实际生效口径（失联/哨兵/stale 判定用的就是这三个值，回显=生效值
// 而非原始 opts，配置写 0/负值时不误导）。三键恒在（无 omitempty）；JSON 加键对
// 既有消费者向后兼容（CLI 镜像 DTO 反序列化忽略未知键，文本渲染不消费）。
type OverviewConfig struct {
	HeartbeatTimeoutSec   int64 `json:"heartbeat_timeout_sec"`    // 会话失联默认阈值（项目级 COALESCE 覆盖前的服务默认）
	SentinelTimeoutSec    int64 `json:"sentinel_timeout_sec"`     // 哨兵失活阈值
	ProgressStaleAfterSec int   `json:"progress_stale_after_sec"` // 进度 stale 黄档阈值秒（红档=2× 由查询侧推导）
}

// Overview status overview 聚合结果（§2.3 data 结构逐字段冻结；CLI --json 与看板同源）。
type Overview struct {
	GeneratedAt      string             `json:"generated_at"`      // 注入时钟（=调用方 now）
	Projects         []ProjectOverview  `json:"projects"`          // 缺省仅 active（all=1 含 archived，总控 #20 裁定；ORDER BY code）
	BlockUnreceipted []BlockUnreceipted `json:"block_unreceipted"` // 全局 block 未回执清单（seq DESC）
	ResourcesSummary ResourcesSummary   `json:"resources_summary"` // in_use 资源摘要
	Config           OverviewConfig     `json:"config"`            // 三阈值生效值回显（B5-6 增补——看板配置展示区数据源）
}

// ProjectOverview §2.3 项目层。
type ProjectOverview struct {
	Code     string           `json:"code"`
	Name     string           `json:"name"`
	Status   string           `json:"status"` // active | archived（archived 仅 all=1 扩面出现）
	Columns  []ColumnOverview `json:"columns"`
	Sessions []SessionEntry   `json:"sessions"`
}

// ColumnOverview §2.3 栏目层。windows 为压缩视图（§2.3 示例形态：key=stage，
// value={status,start,end}；只列已配置启用阶段，未配置栏目=空对象键恒在）——
// B3-5 消费 B4 ListWindowStatus 装配（status_windows_progress.go）。
type ColumnOverview struct {
	Code      string                     `json:"code"`
	Status    string                     `json:"status"`
	Mailboxes []MailboxOverview          `json:"mailboxes"` // 角色字典序
	Windows   map[string]WindowPhaseView `json:"windows"`   // 压缩视图；构造点初始化非 nil（空态 {} 非 null）
}

// MailboxOverview §2.3 信箱层：pending=direct+bus 待消费（chat 不进信箱口径）；
// position=该信箱位点；latest=该信箱最新一条（含已消费，不限位点——「最新到达」非
// 「最新未消费」）；sentinel 三态 alive/dead/none（§7.4：任一哨兵 alive→alive、
// 有行全 dead→dead、无行→none）。
type MailboxOverview struct {
	Role     string         `json:"role"`
	Pending  int64          `json:"pending"`
	Position int64          `json:"position"`
	Latest   *LatestMessage `json:"latest"` // 无消息=null（键恒在）
	Sentinel string         `json:"sentinel"`
}

// LatestMessage 信箱最新一条消息摘要（§2.3 latest 结构：seq/level/created_at 三字段）。
type LatestMessage struct {
	Seq       int64  `json:"seq"`
	Level     string `json:"level"`
	CreatedAt string `json:"created_at"`
}

// SessionEntry 会话行 + 失联判定（§7.2 读时计算不落库；#27 内嵌同源结构超集：
// id/project/sentinels 为 #27 口径字段——「status overview 内嵌同源数据」；
// progress 为 FR23 增补段——AC23.3，指针 omitempty 无上报不出键；
// sentinel_last_hit_at 为 b8-W3 命中留痕透出——空串=从未命中（键恒在）；
// unread_mailbox/unread_dialog 为 b3-W1 未读两分项——看板未读角标数据面，
// 两键恒在（无 omitempty，零消息会话零值透出）。
type SessionEntry struct {
	ID                int64             `json:"id"`
	Project           string            `json:"project"` // 项目 code
	Column            string            `json:"column"`  // 栏目 code
	Name              string            `json:"name"`
	Role              string            `json:"role"`
	LastSeenAt        string            `json:"last_seen_at"`
	Alive             bool              `json:"alive"` // lost = now-last_seen > threshold（恰等不 lost，`>` 口径）
	LostForSec        int64             `json:"lost_for_sec"`
	SentinelLastHitAt string            `json:"sentinel_last_hit_at"` // b8-W3：最近命中时刻（''=从未命中）
	Sentinels         []SessionSentinel `json:"sentinels"`            // 按哨兵 id 序；无哨兵=空数组
	UnreadMailbox     int64             `json:"unread_mailbox"`       // b3-W1：未读信箱数=MailboxOverview.pending 按（栏目,角色）映射（一格多人取同值，零额外查询）
	UnreadDialog      int64             `json:"unread_dialog"`        // b3-W1：未读对话数=Q1 聚合（chat/receipt 排除自发+位点过滤）
	Progress          *SessionProgress  `json:"progress,omitempty"`   // 最近进度（无上报记录不出键）
}

// SessionSentinel #27 sentinels 数组元素（每会话哨兵 id/role/alive——b3-spec #27 行）。
type SessionSentinel struct {
	ID    int64  `json:"id"`
	Role  string `json:"role"`
	Alive bool   `json:"alive"`
}

// BlockUnreceipted §2.3 block 未回执清单元素：target="项目code/栏目code/目标角色"。
type BlockUnreceipted struct {
	Seq       int64  `json:"seq"`
	Level     string `json:"level"` // 恒 "block"
	Target    string `json:"target"`
	Sender    string `json:"sender"` // sender_label 冗余显示
	CreatedAt string `json:"created_at"`
}

// ResourcesSummary §2.3 资源摘要：in_use 行计数 + 按 rtype 分组计数（released 不计；
// 空表=in_use_count 0 + 空 map——json {} 非 null）。
type ResourcesSummary struct {
	InUseCount int64            `json:"in_use_count"`
	ByType     map[string]int64 `json:"by_type"`
}

// ColumnScope 单栏目模式过滤范围（OverviewOpts.Column 非空时生效）。
type ColumnScope struct {
	ProjectCode string
	ColumnCode  string
}

// OverviewOpts BuildOverview 可选项：零值=全局全量聚合。
type OverviewOpts struct {
	// Column 非空=单栏目模式：仅输出该项目该栏目（同一聚合函数带过滤，不另起查询——
	// b3-spec §七「#17 column 同聚合+过滤参数」）。
	Column *ColumnScope
	// IncludeArchived 含 archived 项目（#17 all=1 显式扩面，总控 #20 裁定——b3-spec
	// 「archived 整域排除」为缺省面口径，all=1 为显式扩面非破坏）：项目清单含
	// archived 项目（ORDER BY code 不变、status 原样透出），其下 active 栏目/信箱
	// /会话照常聚合；archived 栏目仍按自身状态排除（--all 语义域=项目生命周期态，
	// 栏目级扩面另行呈批）。与 Column 组合未定义（handler 侧 all 仅 overview 模式
	// 生效，store 不设防）。
	IncludeArchived bool
	// SessionTimeoutSec 失联默认阈值（服务配置 session.heartbeat_timeout_sec 透传位）；
	// 0=默认 900（§7.2）。项目级 heartbeat_timeout_sec 优先于本值（COALESCE 语义）。
	SessionTimeoutSec int64
	// SentinelTimeoutSec 哨兵失活阈值（服务配置 sentinel_timeout_sec 透传位）；
	// 0=默认 15（§7.3）。
	SentinelTimeoutSec int64
	// WindowAsOf 判窗基准 "HH:MM"（handler 经 NowInTz 产生传入，§9.4 D4——与 #22
	// 同源钟面）；空串=跳过 windows 装配（Windows 恒空对象，不发起窗聚合查询）。
	WindowAsOf string
	// ProgressStaleAfterSec 进度 stale 黄档阈值秒（config progress.stale_after 经
	// handler 解析传入，信箱#17③）；0=默认 3600（DefaultProgressStaleAfterSec）；
	// 红档=2× 由 store 自动推导。
	ProgressStaleAfterSec int
}

// boxKey 信箱坐标（栏目 id + 角色）——位点/消息统计/哨兵三态共用的装配键。
type boxKey struct {
	columnID int64
	role     string
}

// projColKey 栏目坐标（项目 id + 栏目 code）——b3-W1 unread_mailbox 映射的会话侧
// 桥接键：sessionRow 只带栏目 code 不带栏目 id（§2.3 sessions[].column 为 code
// 口径），经生效栏目集反查 id 对齐信箱侧 boxKey。
type projColKey struct {
	projectID int64
	code      string
}

// latestStat 聚合产出的信箱最新消息（seq+装配所需 level/created_at）。
type latestStat struct {
	seq       int64
	level     string
	createdAt string
}

// pendingStat 聚合计数（direct/bus 通用）：待消费数 + 信箱最新一条。
type pendingStat struct {
	pending int64
	latest  *latestStat
}

// mailboxAcc 信箱装配累加器：位点/direct/bus/sentinel 四路装配在此汇合，
// 终态经 finalizeMailboxes 转视图归位。
type mailboxAcc struct {
	role     string
	pending  int64
	position int64
	latest   *latestStat // direct 与 bus 两分支的 seq 最大者
	sentinel string      // 三态 alive/dead/none（applySentinels 填充）
}

// BuildOverview 聚合 status overview（§3.7 往返基线，§2.3 结构逐字段装配）：
// 1 projects 全量（缺省 active，all=1 含 archived）/ 2 columns active 全量 / 3 direct
// 待消费计数+latest（GROUP BY 栏目+角色，关联位点）/ 4 bus 待消费计数+latest（controller
// 信箱口径 GROUP BY 项目）/ 5 ack_positions 全量 / 6 sessions 心跳+失联计算列 /
// 7 sentinels 活性（复用 B3-1）/ 8 block 未回执 NOT EXISTS / 9 resources 摘要
// （in_use_count+by_type 一条 GROUP BY）；B3-5 接线增量：10+11 windows 聚合
// （ListWindowStatus 两查，opts.WindowAsOf 空=跳过）/ 12 progress 每会话最新
// （LatestProgressBySession 一查）/ 13 会话未读两分项（b3-W1：unread_dialog=Q1
// 冻结 SQL 单条 IN 聚合——空会话集跳过零往返；unread_mailbox=信箱累加器 pending
// 按（栏目,角色）内存映射零查询）——共 13 条上限，不随项目/栏目数增长（机械闸
// TestRoundtripBudget）。
//
// now=注入时钟（RFC3339 带时区后缀形态，Z 或 ±HH:MM；禁无时区本地墙钟串——SQLite 对
// 无时区串一律按 UTC 解释，本地串会静默偏移）；opts 零值=全局全量+默认阈值。
// 单栏目模式经 opts.Column 过滤装配面，往返数不变；all=1 扩面（opts.IncludeArchived，
// 总控 #20 裁定）仅改项目/会话清单 WHERE，往返数同样不变。
func (s *Store) BuildOverview(now string, opts OverviewOpts) (Overview, error) {
	sessionTimeout := opts.SessionTimeoutSec
	if sessionTimeout == 0 {
		sessionTimeout = defaultSessionTimeoutSec
	}
	sentinelTimeout := opts.SentinelTimeoutSec
	if sentinelTimeout == 0 {
		sentinelTimeout = DefaultSentinelTimeoutSec
	}

	// 往返 1：projects 清单（缺省仅 active；IncludeArchived 含 archived——all=1 扩面，
	// ORDER BY code 稳定输出不变；单栏目模式仅锚定项目）。
	// 项目引用统一存切片下标而非指针：与 sessionRef 同构单一模式——append 扩容迁移底层
	// 数组时 &slice[i] 指针会失效（sessionRef 同族教训），下标引用天然免疫；
	// 后续所有项目级装配一律经 projectIdx 寻址，禁止再存 &projects[i]。
	projectRows, err := s.queryProjects(opts.Column, opts.IncludeArchived)
	if err != nil {
		return Overview{}, err
	}
	projects := make([]ProjectOverview, 0, len(projectRows))
	projectIdx := make(map[int64]int, len(projectRows))      // 表 id → projects 切片下标
	projectCodes := make(map[int64]string, len(projectRows)) // 表 id → code（queryColumns 过滤用）
	for i := range projectRows {
		pr := projectRows[i]
		projects = append(projects, ProjectOverview{
			Code:     pr.code,
			Name:     pr.name,
			Status:   pr.status,
			Columns:  []ColumnOverview{},
			Sessions: []SessionEntry{},
		})
		projectIdx[pr.id] = len(projects) - 1
		projectCodes[pr.id] = pr.code
	}

	// 往返 2：columns 全量 active（archived 栏目按自身状态排除——不随 all=1 项目扩面
	// 放行，--all 语义域=项目生命周期态；archived 项目的 active 栏目在扩面时连带保留，
	// 缺省面经装配丢弃——查询不过滤项目态，双分支共用）。
	columnRows, err := s.queryColumns(opts.Column, projectCodes)
	if err != nil {
		return Overview{}, err
	}
	columnByID := make(map[int64]*columnRow, len(columnRows))
	for i := range columnRows {
		cr := columnRows[i]
		projIdx, ok := projectIdx[cr.projectID]
		if !ok {
			continue // 缺省面 archived 项目的栏目（查询不过滤项目态，装配面丢弃；扩面时项目在列自然保留）
		}
		columnByID[cr.id] = &columnRows[i]
		projects[projIdx].Columns = append(projects[projIdx].Columns, ColumnOverview{
			Code:      cr.code,
			Status:    cr.status,
			Mailboxes: []MailboxOverview{},
			Windows:   map[string]WindowPhaseView{}, // 压缩视图构造点初始化：空态 {} 非 null
		})
	}

	// 往返 3+4+5：信箱口径三查（direct 计数+latest / bus 计数+latest / 位点全量），
	// 装配进累加器（角色集=位点行非 chat: 前缀 consumer ∪ direct 消息出现面——b3-spec #17 行）。
	directs, err := s.queryDirectStats()
	if err != nil {
		return Overview{}, err
	}
	buses, err := s.queryBusStats()
	if err != nil {
		return Overview{}, err
	}
	positions, err := s.queryPositions()
	if err != nil {
		return Overview{}, err
	}
	boxes := assembleMailboxes(directs, buses, positions, columnByID)

	// 往返 6：sessions 心跳+失联（§3.6 冻结 SQL：lost_for_sec 计算列 + COALESCE 阈值；
	// 单栏目模式限定锚定栏目——B3-5 TestColumnMode「限定单栏目输出」语义，同基线带过滤；
	// IncludeArchived 扩面：项目态过滤解除，archived 项目会话随扩面在列——总控 #20 裁定）。
	sessionRows, err := s.querySessions(now, sessionTimeout, opts.Column, opts.IncludeArchived)
	if err != nil {
		return Overview{}, err
	}
	// 会话引用存（项目切片下标, 行下标）而非指针：Sessions 后续 append 扩容会迁移底层数组，
	// 提前取 &slice[i] 指针会在扩容后失效（哨兵装配写不进最终视图）。
	sessionByID := make(map[int64]sessionRef, len(sessionRows))
	for _, sr := range sessionRows {
		projIdx, ok := projectIdx[sr.projectID]
		if !ok {
			continue // 防御：缺省面 SQL 已滤 active 项目；扩面/单栏目范围外的会话行丢弃
		}
		sessions := &projects[projIdx].Sessions
		*sessions = append(*sessions, SessionEntry{
			ID:                sr.id,
			Project:           sr.projectCode,
			Column:            sr.columnCode,
			Name:              sr.name,
			Role:              sr.role,
			LastSeenAt:        sr.lastSeenAt,
			SentinelLastHitAt: sr.sentinelLastHitAt, // b8-W3 命中留痕透出（''=从未命中）
			LostForSec:        sr.lostForSec,
			Alive:             sr.lostForSec <= sr.threshold, // lost=`>` 严格大于，恰等不 lost（§7.2）
			Sentinels:         []SessionSentinel{},
		})
		sessionByID[sr.id] = sessionRef{projIdx: projIdx, index: len(*sessions) - 1}
	}

	// 往返 7：sentinels 活性（复用 B3-1 全量+alive 标志；同一清单两处装配零额外查询）：
	// a) mailboxes[].sentinel 三态 b) sessions[].sentinels 按 session 分组（#27 口径）。
	sentinels, err := s.ListSentinelsWithLiveness(now, sentinelTimeout)
	if err != nil {
		return Overview{}, err
	}
	applySentinels(boxes, sessionByID, sentinels, columnByID, projects)
	finalizeMailboxes(boxes, columnByID, projectIdx, projects)

	// 往返 8：block 未回执清单（§3.6 NOT EXISTS 冻结 SQL 全局口径——聚合无 :me 维度）。
	unreceipted, err := s.queryUnreceiptedBlocks()
	if err != nil {
		return Overview{}, err
	}

	// 往返 9：resources 摘要（一条 GROUP BY 同产 in_use_count 与 by_type）。
	summary, err := s.queryResourcesSummary()
	if err != nil {
		return Overview{}, err
	}

	// 往返 10+11（B3-5 接线）：B4 窗聚合压缩装配（opts.WindowAsOf 空=跳过——
	// 不判窗场景不发起两查）；单栏目模式经 scopeProjectCode 限定项目、生效范围外
	// 栏目由归属索引落空。
	if opts.WindowAsOf != "" {
		winEntries, err := s.ListWindowStatus(opts.WindowAsOf, scopeProjectCode(opts.Column))
		if err != nil {
			return Overview{}, err
		}
		assembleWindows(projects, winEntries)
	}

	// 往返 12（B3-5 接线）：FR23 progress 每会话最新（stale 三态已由查询侧算好，
	// 按 SessionID 内存装配——禁 N+1）。单栏目模式 progress 全量拉+生效范围外经
	// sessionByID 内存落空为接受口径（往返恒 1 条不破闸；范围过滤需改 B8 产物
	// LatestProgressBySession 签名=跨批改动，归 B3-8 裁量）——与 windows 侧
	// ListWindowStatus 自带 projectCode 过滤参数的不对称在此显式声明。
	progressReports, err := s.LatestProgressBySession(now, resolveStaleAfterSec(opts.ProgressStaleAfterSec))
	if err != nil {
		return Overview{}, err
	}
	assembleProgress(projects, sessionByID, progressReports)

	// 往返 13（b3-W1）：会话未读两分项——unread_dialog=Q1 单条 IN 聚合（tech-design
	// §3.6 冻结 SQL；空会话集跳过查询零往返），unread_mailbox=信箱累加器 pending 按
	// （栏目,角色）内存映射（同格多人取同值，与 MailboxOverview.pending 同源同值，
	// 零额外查询）。栏目侧桥接：boxKey 用栏目 id 而会话行只带栏目 code（§2.3
	// sessions[].column 口径），经生效栏目集建（项目,栏目code）→栏目id 查找表对齐
	// （archived 栏目会话落空映射 0=其信箱本就不装配，同口径）。
	var unread map[int64]int64 // 会话→未读对话数（nil/缺键=0，nil map 读合法）
	if len(sessionByID) > 0 {
		ids := make([]int64, 0, len(sessionByID))
		for id := range sessionByID {
			ids = append(ids, id)
		}
		slices.Sort(ids) // IN 列表稳定序——同会话集产出同 SQL 文本，利于语句缓存
		unread, err = s.queryUnreadDialogCounts(ids)
		if err != nil {
			return Overview{}, err
		}
	}
	columnIDOf := make(map[projColKey]int64, len(columnByID))
	for _, cr := range columnByID {
		columnIDOf[projColKey{projectID: cr.projectID, code: cr.code}] = cr.id
	}
	for _, sr := range sessionRows {
		ref, ok := sessionByID[sr.id]
		if !ok {
			continue // 装配面已丢弃的会话行（archived 域/单栏目范围外）同样跳过未读填充
		}
		se := &projects[ref.projIdx].Sessions[ref.index]
		se.UnreadDialog = unread[sr.id]
		if a := boxes[boxKey{columnID: columnIDOf[projColKey{projectID: sr.projectID, code: sr.columnCode}], role: sr.role}]; a != nil {
			se.UnreadMailbox = a.pending
		}
	}

	return Overview{
		GeneratedAt:      now,
		Projects:         projects,
		BlockUnreceipted: unreceipted,
		ResourcesSummary: summary,
		// config 回显段（B5-6）：填兜底后的生效值（sessionTimeout/sentinelTimeout
		// 局部变量即失联/哨兵判定实收口径；stale 阈值与往返 12 消费同取
		// resolveStaleAfterSec——单一来源，不另算一遍）。填充点在 store 侧而非
		// handler：opts 原值在配置写 0/负时与生效值偏离（0=兜底 900/15），回显
		// opts 原值会误导；CLI 直调 BuildOverview 场景（--json 同构镜像）与 #17
		// 天然同源。handler 零改动=调用面零变化。
		Config: OverviewConfig{
			HeartbeatTimeoutSec:   sessionTimeout,
			SentinelTimeoutSec:    sentinelTimeout,
			ProgressStaleAfterSec: resolveStaleAfterSec(opts.ProgressStaleAfterSec),
		},
	}, nil
}

// assembleMailboxes 信箱装配：direct/bus/位点三路查询汇入累加器，按（栏目, 角色）合流。
// pending=direct 计数（controller 信箱另加 bus 计数）；position=位点行值（缺行=0）；
// latest=direct 与 bus 两分支 seq 最大者（不限位点——含已消费）。
// 超出生效范围（archived 域/单栏目模式）的统计行按栏目归属表丢弃。
// 哨兵三态与视图归位分别由 applySentinels / finalizeMailboxes 后续完成。
func assembleMailboxes(directs map[boxKey]pendingStat, buses map[int64]pendingStat,
	positions map[boxKey]int64, columnByID map[int64]*columnRow) map[boxKey]*mailboxAcc {
	acc := make(map[boxKey]*mailboxAcc)
	ensure := func(k boxKey) *mailboxAcc {
		if a, ok := acc[k]; ok {
			return a
		}
		a := &mailboxAcc{role: k.role}
		acc[k] = a
		return a
	}
	for k, st := range directs {
		if _, live := columnByID[k.columnID]; !live {
			continue
		}
		a := ensure(k)
		a.pending += st.pending
		a.latest = st.latest
	}
	for k, pos := range positions {
		if _, live := columnByID[k.columnID]; !live {
			continue
		}
		a := ensure(k)
		a.position = pos
	}
	for projectID, st := range buses {
		// bus 待消费并入该项目每个 controller 信箱（冻结口径 GROUP BY project_id——
		// 单栏目 controller 常态下与 poll 可见性一致；多栏目 controller 为已知近似：
		// B3-8 收敛对审确认保留——精确对齐需按栏目各自位点逐栏目聚合，与 §3.7
		// 单条 GROUP BY 往返基线冲突，近似误差=多栏目 controller 视图重复计入，
		// 修法归看板域需求批再议）。
		for k, a := range acc {
			if k.role != "controller" {
				continue
			}
			cr := columnByID[k.columnID]
			if cr == nil || cr.projectID != projectID {
				continue
			}
			a.pending += st.pending
			if a.latest == nil || st.latest.seq > a.latest.seq {
				a.latest = st.latest // controller 信箱 latest=direct/bus 两分支最新
			}
		}
	}
	return acc
}

// sessionRef 会话行在聚合视图中的定位（项目切片下标 projIdx + 行下标 index）——与项目
// 下标引用同构单一模式：存下标不存指针，append 扩容迁移底层数组天然免疫。
type sessionRef struct {
	projIdx int
	index   int
}

// applySentinels 哨兵清单二次装配（零额外查询）：mailboxes[].sentinel 三态 +
// sessions[].sentinels 按 session 分组（超出生效范围的哨兵行丢弃）。
func applySentinels(acc map[boxKey]*mailboxAcc, sessionByID map[int64]sessionRef,
	sentinels []Sentinel, columnByID map[int64]*columnRow, projects []ProjectOverview) {
	boxAlive := make(map[boxKey]bool)
	boxHasRow := make(map[boxKey]bool)
	for _, st := range sentinels {
		k := boxKey{columnID: st.ColumnID, role: st.Role}
		if _, live := columnByID[k.columnID]; !live {
			continue
		}
		boxHasRow[k] = true
		if st.Alive {
			boxAlive[k] = true
		}
		if ref, ok := sessionByID[st.SessionID]; ok {
			sessions := projects[ref.projIdx].Sessions
			sessions[ref.index].Sentinels = append(sessions[ref.index].Sentinels,
				SessionSentinel{ID: st.ID, Role: st.Role, Alive: st.Alive})
		}
	}
	for k, a := range acc {
		switch {
		case boxAlive[k]:
			a.sentinel = "alive" // 任一哨兵存活
		case boxHasRow[k]:
			a.sentinel = "dead" // 有行全 dead
		default:
			a.sentinel = "none" // 无哨兵行
		}
	}
}

// finalizeMailboxes 累加器终态转 §2.3 信箱视图：按栏目归位、角色字典序排序（输出稳定）。
func finalizeMailboxes(acc map[boxKey]*mailboxAcc, columnByID map[int64]*columnRow,
	projectIdx map[int64]int, projects []ProjectOverview) {
	columnBoxes := make(map[int64][]MailboxOverview)
	for k, a := range acc {
		mb := MailboxOverview{
			Role:     a.role,
			Pending:  a.pending,
			Position: a.position,
			Sentinel: a.sentinel,
		}
		if a.latest != nil {
			mb.Latest = &LatestMessage{Seq: a.latest.seq, Level: a.latest.level, CreatedAt: a.latest.createdAt}
		}
		columnBoxes[k.columnID] = append(columnBoxes[k.columnID], mb)
	}
	for columnID, list := range columnBoxes {
		cr := columnByID[columnID]
		po := &projects[projectIdx[cr.projectID]] // columnByID 仅收装配成功栏目，项目必命中
		slices.SortFunc(list, func(a, b MailboxOverview) int { return strings.Compare(a.Role, b.Role) })
		for ci := range po.Columns {
			if po.Columns[ci].Code == cr.code {
				po.Columns[ci].Mailboxes = list
				break
			}
		}
	}
}

// projectRow 往返 1 行视图。
type projectRow struct {
	id     int64
	code   string
	name   string
	status string
}

// queryProjects 往返 1：projects 清单（缺省仅 active——§2.3 冻结口径，SQL 文本不变；
// includeArchived=all=1 显式扩面含 archived，总控 #20 裁定；单栏目模式限定锚定项目
// ——同基线带过滤）。
func (s *Store) queryProjects(scope *ColumnScope, includeArchived bool) ([]projectRow, error) {
	q := `SELECT id, code, name, status FROM projects`
	if !includeArchived {
		q += ` WHERE status = 'active'`
	}
	q += ` ORDER BY code`
	rows, err := s.DB.Query(q)
	if err != nil {
		return nil, fmt.Errorf("store: status 项目查询失败: %w", err)
	}
	defer rows.Close()
	var out []projectRow
	for rows.Next() {
		var r projectRow
		if err := rows.Scan(&r.id, &r.code, &r.name, &r.status); err != nil {
			return nil, fmt.Errorf("store: 扫描 status 项目行失败: %w", err)
		}
		if scope != nil && r.code != scope.ProjectCode {
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历 status 项目行失败: %w", err)
	}
	return out, nil
}

// columnRow 往返 2 行视图。
type columnRow struct {
	id        int64
	projectID int64
	code      string
	status    string
}

// queryColumns 往返 2：columns 全量 active（单栏目模式限定锚定栏目）。archived 栏目
// 按自身状态恒排除——不随 all=1 项目扩面放行（--all 语义域=项目生命周期态，见
// OverviewOpts.IncludeArchived 注释）；archived 项目的 active 栏目缺省面经装配丢弃、
// 扩面时项目在列自然保留（BuildOverview 往返 2 装配 continue 分支）。
func (s *Store) queryColumns(scope *ColumnScope, projectCodes map[int64]string) ([]columnRow, error) {
	rows, err := s.DB.Query(`SELECT id, project_id, code, status FROM columns WHERE status = 'active' ORDER BY project_id, code`)
	if err != nil {
		return nil, fmt.Errorf("store: status 栏目查询失败: %w", err)
	}
	defer rows.Close()
	var out []columnRow
	for rows.Next() {
		var r columnRow
		if err := rows.Scan(&r.id, &r.projectID, &r.code, &r.status); err != nil {
			return nil, fmt.Errorf("store: 扫描 status 栏目行失败: %w", err)
		}
		if scope != nil && (projectCodes[r.projectID] != scope.ProjectCode || r.code != scope.ColumnCode) {
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历 status 栏目行失败: %w", err)
	}
	return out, nil
}

// queryDirectStats 往返 3：direct 待消费计数 + 信箱最新一条（含已消费；MAX(seq) JOIN 回
// 原 message 行带出 level/created_at）。
//
// B3-8 收敛裁定（b3-spec §六）：保留自含实现，不收敛为 B2 PollVisible——
//   - 形态不匹配：PollVisible 是单 (project,column,role) 信箱的行级拉取（返回
//     []Message），本查询是全表 GROUP BY(栏目,角色) 的 SUM(pending)+MAX(seq) 聚合，
//     一次往返出全部信箱计数；改调 PollVisible 须逐信箱一查=N+1，违反 §3.7 往返
//     基线「不随栏目数增长」（机械闸 TestRoundtripBudget 打回）。
//   - 谓词语义同源：本 SQL 聚合形态 `SUM(CASE WHEN m.seq > COALESCE(p.position,0)
//     THEN 1 ELSE 0 END)` 即技术设计 §3.6 原文「待消费数（status 聚合）：与上同
//     谓词的 COUNT(*)」——被聚合的行过滤谓词（m.kind='direct' AND m.column_id=:c
//     AND m.target_role=:r AND m.seq>:pos_m）与 B2 pollVisibleSQL direct 分支同源
//     （行过滤语义一致，非逐字同文——彼为行级拉取、此为聚合计数改写形态），位点
//     join（LEFT JOIN ack_positions，consumer=角色）同 B2 GetPositions 读语义
//     （缺行 COALESCE 0）——双路径已显式登记，非静默。
func (s *Store) queryDirectStats() (map[boxKey]pendingStat, error) {
	rows, err := s.DB.Query(`
SELECT g.column_id, g.target_role, g.pending,
       m.seq, m.level, m.created_at
FROM (
  SELECT m.column_id, m.target_role,
         SUM(CASE WHEN m.seq > COALESCE(p.position, 0) THEN 1 ELSE 0 END) AS pending,
         MAX(m.seq) AS latest_seq
  FROM messages m
  LEFT JOIN ack_positions p
    ON p.column_id = m.column_id AND p.consumer = m.target_role
  WHERE m.kind = 'direct'
  GROUP BY m.column_id, m.target_role
) g
JOIN messages m ON m.seq = g.latest_seq`)
	if err != nil {
		return nil, fmt.Errorf("store: status direct 计数查询失败: %w", err)
	}
	defer rows.Close()
	out := make(map[boxKey]pendingStat)
	for rows.Next() {
		var (
			k  boxKey
			st pendingStat
			lv latestStat
		)
		if err := rows.Scan(&k.columnID, &k.role, &st.pending, &lv.seq, &lv.level, &lv.createdAt); err != nil {
			return nil, fmt.Errorf("store: 扫描 direct 计数行失败: %w", err)
		}
		st.latest = &lv
		out[k] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历 direct 计数行失败: %w", err)
	}
	return out, nil
}

// queryBusStats 往返 4：bus 待消费计数 + 最新一条（controller 信箱口径，GROUP BY project_id）。
//
// B3-8 收敛裁定（b3-spec §六）：保留自含实现，不收敛为 B2 PollVisible——理由同
// queryDirectStats（聚合计数形态 vs 行级拉取，替换即 N+1 破 §3.7 往返基线）；
// 聚合形态同出技术设计 §3.6「待消费数（status 聚合）：与上同谓词的 COUNT(*)」，
// 行过滤谓词（m.kind='bus' AND m.project_id=:p AND m.seq>:pos_m AND 角色限
// controller）与 B2 pollVisibleSQL bus 分支语义同源（非逐字同文），位点 join 维度
// 差异（此处 join 消息所在栏目位点，poll 用消费方传入位点）在单栏目 controller
// 常态下等值，多栏目 controller 为已知近似（assembleMailboxes 注显式声明）。
func (s *Store) queryBusStats() (map[int64]pendingStat, error) {
	rows, err := s.DB.Query(`
SELECT g.project_id, g.pending,
       m.seq, m.level, m.created_at
FROM (
  SELECT m.project_id,
         SUM(CASE WHEN m.seq > COALESCE(p.position, 0) THEN 1 ELSE 0 END) AS pending,
         MAX(m.seq) AS latest_seq
  FROM messages m
  LEFT JOIN ack_positions p
    ON p.column_id = m.column_id AND p.consumer = 'controller'
  WHERE m.kind = 'bus'
  GROUP BY m.project_id
) g
JOIN messages m ON m.seq = g.latest_seq`)
	if err != nil {
		return nil, fmt.Errorf("store: status bus 计数查询失败: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]pendingStat)
	for rows.Next() {
		var (
			projectID int64
			st        pendingStat
			lv        latestStat
		)
		if err := rows.Scan(&projectID, &st.pending, &lv.seq, &lv.level, &lv.createdAt); err != nil {
			return nil, fmt.Errorf("store: 扫描 bus 计数行失败: %w", err)
		}
		st.latest = &lv
		out[projectID] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历 bus 计数行失败: %w", err)
	}
	return out, nil
}

// queryPositions 往返 5：ack_positions 全量（信箱位点=角色 consumer；chat: 前缀为会话
// 对话位点，信箱口径排除——b3-spec #17 行「非 chat: 前缀 consumer」）。
//
// B3-8 收敛裁定（b3-spec §六）：保留自含实现，不收敛为 B2 GetPositions——
//   - 形态不匹配：GetPositions 是单 (column,role[,session]) 点读且带写副作用
//     （信箱行缺失惰性 upsert=MAX(seq)），本查询是全表一条 SELECT 的纯读装配面；
//     改调 GetPositions 须逐信箱一查=N+1 破 §3.7 往返基线，且 status 读路径被
//     植入位点落行写面（聚合须零写）。
//   - 语义有意分化：此处位点缺行不落行（读路径零写），与 GetPositions 惰性
//     初始化（写）分属读/写两侧口径——同表不同消费面，非双胞胎。
func (s *Store) queryPositions() (map[boxKey]int64, error) {
	rows, err := s.DB.Query(`SELECT column_id, consumer, position FROM ack_positions`)
	if err != nil {
		return nil, fmt.Errorf("store: status 位点查询失败: %w", err)
	}
	defer rows.Close()
	out := make(map[boxKey]int64)
	for rows.Next() {
		var (
			columnID int64
			consumer string
			position int64
		)
		if err := rows.Scan(&columnID, &consumer, &position); err != nil {
			return nil, fmt.Errorf("store: 扫描位点行失败: %w", err)
		}
		if strings.HasPrefix(consumer, chatPositionPrefix) {
			continue // 对话位点不进信箱口径
		}
		out[boxKey{columnID: columnID, role: consumer}] = position
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历位点行失败: %w", err)
	}
	return out, nil
}

// sessionRow 往返 6 行视图（lost_for_sec/threshold 为 SQL 计算列；sentinelLastHitAt
// 为 b8-W3 命中留痕列——同查询带出，零额外往返）。
type sessionRow struct {
	id                int64
	projectID         int64
	projectCode       string
	columnCode        string
	name              string
	role              string
	lastSeenAt        string
	sentinelLastHitAt string
	lostForSec        int64
	threshold         int64
}

// querySessions 往返 6：sessions 心跳+失联（§3.6 冻结 SQL：阈值=COALESCE(项目级,
// 服务默认)；lost_for_sec 计算列读时不落库；strftime('%s','now') 的 'now' 字面量改绑定
// 参数注入）。多带出 p.code/c.code 两条展示列（§2.3/#27 sessions[].project/column 为 code）。
// scope 非 nil=单栏目模式限定锚定栏目（项目+栏目双条件——跨项目同名栏目不串台）；
// includeArchived=all=1 扩面：项目态过滤解除（archived 项目会话随扩面在列，总控 #20
// 裁定），缺省面 WHERE 文本不变。
func (s *Store) querySessions(now string, defaultTimeout int64, scope *ColumnScope, includeArchived bool) ([]sessionRow, error) {
	q := `
SELECT s.id, s.project_id, p.code, c.code, s.name, s.role, s.last_seen_at,
       s.sentinel_last_hit_at,
       (strftime('%s', ?) - strftime('%s', s.last_seen_at)) AS lost_for_sec,
       COALESCE(p.heartbeat_timeout_sec, ?) AS threshold
FROM sessions s
JOIN projects p ON p.id = s.project_id
JOIN columns c ON c.id = s.column_id`
	args := []any{now, defaultTimeout}
	var conds []string
	if !includeArchived {
		conds = append(conds, `p.status = 'active'`)
	}
	if scope != nil {
		conds = append(conds, `p.code = ?`, `c.code = ?`)
		args = append(args, scope.ProjectCode, scope.ColumnCode)
	}
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, " AND ")
	}
	q += ` ORDER BY s.id`
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: status 会话查询失败: %w", err)
	}
	defer rows.Close()
	var out []sessionRow
	for rows.Next() {
		var r sessionRow
		if err := rows.Scan(&r.id, &r.projectID, &r.projectCode, &r.columnCode, &r.name,
			&r.role, &r.lastSeenAt, &r.sentinelLastHitAt, &r.lostForSec, &r.threshold); err != nil {
			return nil, fmt.Errorf("store: 扫描 status 会话行失败: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历 status 会话行失败: %w", err)
	}
	return out, nil
}

// queryUnreceiptedBlocks 往返 8：发送方 block 未回执清单（§3.6 冻结 SQL：
// level='block' AND NOT EXISTS(回执) ORDER BY seq DESC；全局聚合口径无 :me 维度）。
// 多带出项目/栏目 code 两条装配列（§2.3 target="proj/col/role" 结构所需）。
//
// B3-8 收敛裁定（b3-spec §六）：保留自含实现，不收敛为 B2 ListUnreceipted——
//   - 口径不匹配：ListUnreceipted(senderSessionID) 按 :me 发送方过滤（「我」发出
//     的 block），本清单是全局全发送方口径（看板「阻断未回执」问的是全场谁被
//     阻断——技术设计 §4.2 --global 样例两发送方行 #881<-controller-A@05 与
//     #884<-controller-A@06、V3 四问③全场视角）——传任一具体 sender 都截断
//     清单，无法经既有签名复用。
//   - select 面不匹配：status 需 JOIN projects/columns 带出 proj/col code 装配
//     target 三段式，ListUnreceipted 无此两列。
//   - 谓词同源：`level='block' AND NOT EXISTS(SELECT 1 FROM message_receipts …)
//     ORDER BY seq DESC` 与 §3.6/B2 receipts.go 逐字一致，双路径显式登记非静默。
func (s *Store) queryUnreceiptedBlocks() ([]BlockUnreceipted, error) {
	rows, err := s.DB.Query(`
SELECT m.seq, m.sender_label, m.target_role, m.created_at,
       pr.code, c.code
FROM messages m
JOIN projects pr ON pr.id = m.project_id
JOIN columns c ON c.id = m.column_id
WHERE m.level = 'block'
  AND NOT EXISTS (SELECT 1 FROM message_receipts r WHERE r.message_seq = m.seq)
ORDER BY m.seq DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: status 未回执查询失败: %w", err)
	}
	defer rows.Close()
	out := []BlockUnreceipted{}
	for rows.Next() {
		var (
			b           BlockUnreceipted
			targetRole  string
			projectCode string
			columnCode  string
		)
		if err := rows.Scan(&b.Seq, &b.Sender, &targetRole, &b.CreatedAt, &projectCode, &columnCode); err != nil {
			return nil, fmt.Errorf("store: 扫描未回执行失败: %w", err)
		}
		b.Level = "block"
		b.Target = projectCode + "/" + columnCode + "/" + targetRole
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历未回执行失败: %w", err)
	}
	return out, nil
}

// queryResourcesSummary 往返 9：resources 摘要——一条 GROUP BY 同产 by_type 与 in_use_count
// （Go 侧求和；released 不计；空表=空 map，json {} 非 null）。
func (s *Store) queryResourcesSummary() (ResourcesSummary, error) {
	rows, err := s.DB.Query(`SELECT rtype, COUNT(*) FROM resources WHERE status = 'in_use' GROUP BY rtype`)
	if err != nil {
		return ResourcesSummary{}, fmt.Errorf("store: status 资源摘要查询失败: %w", err)
	}
	defer rows.Close()
	summary := ResourcesSummary{ByType: make(map[string]int64)}
	for rows.Next() {
		var (
			rtype string
			n     int64
		)
		if err := rows.Scan(&rtype, &n); err != nil {
			return ResourcesSummary{}, fmt.Errorf("store: 扫描资源摘要行失败: %w", err)
		}
		summary.ByType[rtype] = n
		summary.InUseCount += n
	}
	if err := rows.Err(); err != nil {
		return ResourcesSummary{}, fmt.Errorf("store: 遍历资源摘要行失败: %w", err)
	}
	return summary, nil
}

// queryUnreadDialogCounts 往返 13（b3-W1）：会话未读对话聚合——Q1 冻结 SQL 逐字
// 采用（tech-design §3.6，谓词不得改写）：kind∈(chat,receipt) 且排除自发
// （sender_session_id != target_session_id——poll 第三分支同款「自发不回流」语义）
// 且 seq>对话位点（consumer=chat:session:<目标会话>，LEFT JOIN 缺行 COALESCE 0=
// 全量可见）；GROUP BY target_session_id，sessionIDs 动态 IN 占位符（空集函数内
// 早退零往返，调用方 BuildOverview 另有跳过守卫双防线），一条查询出全部会话
// 计数，结果 map 无命中=0。
//
// 与 B2 PollVisible 的关系同本文件既有裁定（b3-spec §六「SQL 文本一致时允许保留
// 但须注明」）：PollVisible 是单消费上下文的行级拉取，本查询是全会话 GROUP BY
// 聚合纯读形态，改调即 N+1 破 §3.7 往返基线；行过滤谓词与 pollVisibleSQL 第三
// 分支（messages.go）同源一致，双路径显式登记非静默——对拍机械闸
// TestUnreadDialogMatchesPollPredicate。
func (s *Store) queryUnreadDialogCounts(sessionIDs []int64) (map[int64]int64, error) {
	if len(sessionIDs) == 0 {
		return map[int64]int64{}, nil // 空集早退——IN () 为 SQL 语法错，调用方守卫外再一道自洽防线
	}
	placeholders := strings.TrimRight(strings.Repeat("?, ", len(sessionIDs)), ", ")
	q := `
SELECT m.target_session_id, COUNT(*)
FROM messages m LEFT JOIN ack_positions p
  ON p.column_id = m.column_id AND p.consumer = 'chat:session:' || m.target_session_id
WHERE m.kind IN ('chat','receipt') AND m.sender_session_id != m.target_session_id
  AND m.seq > COALESCE(p.position, 0) AND m.target_session_id IN (` + placeholders + `)
GROUP BY m.target_session_id`
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		args[i] = id
	}
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 会话未读聚合查询失败: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]int64, len(sessionIDs))
	for rows.Next() {
		var (
			sessionID int64
			n         int64
		)
		if err := rows.Scan(&sessionID, &n); err != nil {
			return nil, fmt.Errorf("store: 扫描会话未读聚合行失败: %w", err)
		}
		out[sessionID] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历会话未读聚合行失败: %w", err)
	}
	return out, nil
}
