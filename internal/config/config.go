package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// dbTypeSQLite 一期唯一支持的数据库类型（技术设计 §3.1 第 7 条）。
const dbTypeSQLite = "sqlite"

// ServerConfig 服务端配置（技术设计 §9.3）。
// 全部字段可省略：加载时未出现的字段保持 Default() 的值。
type ServerConfig struct {
	Listen   string         `json:"listen"`   // HTTP 监听地址，如 0.0.0.0:8310
	DB       DBConfig       `json:"db"`       // 数据库配置
	Auth     AuthConfig     `json:"auth"`     // 鉴权配置
	Session  SessionConfig  `json:"session"`  // 会话超时配置
	Watch    WatchConfig    `json:"watch"`    // 文件监听配置
	Board    BoardConfig    `json:"board"`    // 看板轮询配置
	Progress ProgressConfig `json:"progress"` // 进度上报配置（b8-spec §1.1）
	Timezone string         `json:"timezone"` // 时区，如 Local / Asia/Shanghai
}

// DBConfig 数据库配置。db.type 一期仅支持 sqlite。
type DBConfig struct {
	Type string `json:"type"` // 数据库类型，一期仅 "sqlite"
	Path string `json:"path"` // sqlite 数据库文件路径
}

// AuthConfig 鉴权配置。
type AuthConfig struct {
	TokenEnabled bool   `json:"token_enabled"` // 是否启用 Bearer Token 校验
	Token        string `json:"token"`         // 校验用 Token，TokenEnabled 时必填
}

// SessionConfig 会话配置。
type SessionConfig struct {
	HeartbeatTimeoutSec int `json:"heartbeat_timeout_sec"` // 心跳超时秒数，超时判会话离线
}

// WatchConfig 文件监听配置。
type WatchConfig struct {
	PollIntervalSec    int `json:"poll_interval_sec"`    // 轮询间隔秒数
	SentinelTimeoutSec int `json:"sentinel_timeout_sec"` // 哨兵文件超时秒数
}

// BoardConfig 看板配置。
type BoardConfig struct {
	PollIntervalSec int `json:"poll_interval_sec"` // 看板轮询间隔秒数
}

// ProgressConfig 进度上报配置（b8-spec §1.1，FR23）。
type ProgressConfig struct {
	StaleAfter string `json:"stale_after"` // 上报失联判定阈值（时长字符串，如 60m）：超 1×黄标/2×红标（AC23.3）
	AutoHook   bool   `json:"auto_hook"`   // init 是否自动安装 post-commit hook（进度自动上报层，AC23.1）
}

// Default 返回内建默认配置（§9.3 样例值）。无配置文件时服务以此启动（AC17.2）。
// DB.Path 为相对值（db/aiteam.db）：本包不做路径解析，由 serve 层相对 exe 所在
// 目录（部署根）解析——序 9 部署结构拍平后 CWD 基准退役。
func Default() *ServerConfig {
	return &ServerConfig{
		Listen: "0.0.0.0:8310",
		DB:     DBConfig{Type: dbTypeSQLite, Path: "db/aiteam.db"},
		Auth:   AuthConfig{TokenEnabled: false, Token: ""},
		Session: SessionConfig{
			HeartbeatTimeoutSec: 900,
		},
		Watch: WatchConfig{
			PollIntervalSec:    5,
			SentinelTimeoutSec: 15,
		},
		Board: BoardConfig{
			PollIntervalSec: 5,
		},
		Progress: ProgressConfig{
			StaleAfter: "60m",
			AutoHook:   true,
		},
		Timezone: "Local",
	}
}

// Load 从 path 读取 JSON 配置，叠加到默认值上。
//
//   - path 为空或文件不存在：返回默认配置且无错误（AC17.2）；
//   - 文件存在：json.Unmarshal 只覆盖 JSON 中出现的字段（嵌套子结构同理），
//     未出现的字段保持默认；未知字段（如 _doc）默认忽略不报错；
//     UTF-8 BOM 自动剥离（中文 Windows 记事本等编辑器高发）；
//   - 读取失败（权限等）、内容非法 JSON 或空文件/纯空白（解析失败）：
//     返回错误拒启——区别于文件不存在走默认。
func Load(path string) (*ServerConfig, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}
	// 剥离 UTF-8 BOM，否则 json.Unmarshal 报 invalid character '\ufeff'。
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}
	return cfg, nil
}

// Validate 校验配置合法性：db.type 白名单（一期仅 sqlite）；listen 不可为空；
// token 开启但为空拒启（B6-1——开启校验却无可用密钥=全部数据口必然 401 的错误
// 配置，启动期拒绝优于运行期全拒）。每次启动 Load 一次后调用，不通过则拒绝启动
// （AC17.1 配套约束）。
func (c *ServerConfig) Validate() error {
	if c.DB.Type != dbTypeSQLite {
		return fmt.Errorf("db.type 一期仅 sqlite，当前为 %q", c.DB.Type)
	}
	if strings.TrimSpace(c.Listen) == "" {
		return errors.New("listen 不能为空")
	}
	if c.Auth.TokenEnabled && strings.TrimSpace(c.Auth.Token) == "" {
		return errors.New("token 开启但为空：auth.token_enabled=true 时 auth.token 必填")
	}
	return nil
}
