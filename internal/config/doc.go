// Package config 提供服务端配置的加载与默认值。
//
// Load 从 JSON 文件读取 ServerConfig：缺省字段回退内建默认（Default），
// 未知字段忽略；Validate 校验 db.type 白名单与 listen 非空。
// serve 子命令启动前 Load 一次并校验，不通过则拒绝启动（技术设计 §9.3）。
package config
