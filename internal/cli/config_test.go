package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aiteam/internal/config"
)

// runConfigInit 便利封装：chdir 进 dir 后执行 config init，返回退出码/stdout/
// stderr（复用 serve_test 的 chdirForTest 与 register_test 的 captureStdoutStderr
// 基建；退出码分流同 main 层 configCmd：ErrUsage=2 其余=1）。
func runConfigCmd(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	chdirForTest(t, dir)
	var code int
	var runErr error
	stdout, stderr := captureStdoutStderr(t, func() {
		runErr = RunConfig(args)
	})
	switch {
	case runErr == nil:
		code = 0
	case errors.Is(runErr, ErrUsage):
		code = 2
		stderr += runErr.Error()
	default:
		code = 1
		stderr += runErr.Error()
	}
	return code, stdout, stderr
}

// stripDocKeys 递归剥除 _doc 说明键（生成物与 Default() 对拍前统一形态）。
func stripDocKeys(m map[string]any) {
	for k, v := range m {
		if k == "_doc" {
			delete(m, k)
			continue
		}
		if sub, ok := v.(map[string]any); ok {
			stripDocKeys(sub)
		}
	}
}

// jsonTagsAllLevels 反射收集 ServerConfig 全字段 json tag（顶层+嵌套 struct
// 字段），返回顶层 tag 集+各段 tag 集——生成内容对拍 Config 结构体全集的
// 机械判据：将来 Config 加字段忘更新生成模板，此处即红。
func jsonTagsAllLevels(t *testing.T) (topTags []string, sectionTags map[string][]string) {
	t.Helper()
	sectionTags = map[string][]string{}
	typ := reflect.TypeOf(config.ServerConfig{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		topTags = append(topTags, tag)
		ft := f.Type
		if ft.Kind() == reflect.Struct {
			for j := 0; j < ft.NumField(); j++ {
				sub := ft.Field(j)
				sectionTags[tag] = append(sectionTags[tag], strings.Split(sub.Tag.Get("json"), ",")[0])
			}
		}
	}
	return topTags, sectionTags
}

// TestConfigInitGeneratesDefaults 生成内容对拍 Config 结构体全集（#11）：退 0
// +stdout 含「重启 serve 生效」提示+文件在 CWD 生成；生成 JSON 经 _doc 剥除后
// 与 config.Default() 序列化深比较（值面零漂移），且顶层/各段 json tag 全集
// 齐备（结构面零缺漏——Config 演进时模板漏更即红）。
func TestConfigInitGeneratesDefaults(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := runConfigCmd(t, dir, "init")
	if code != 0 {
		t.Fatalf("退码 = %d，期望 0（stderr: %s）", code, stderr)
	}
	if !strings.Contains(stdout, "重启 serve 生效") {
		t.Errorf("stdout 缺「重启 serve 生效」提示: %q", stdout)
	}
	data, err := os.ReadFile(filepath.Join(dir, "aiteam-config.json"))
	if err != nil {
		t.Fatalf("读取生成文件失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("生成内容非法 JSON: %v", err)
	}
	// 结构面：顶层 tag 全集+_doc 说明字段在位。
	topTags, sectionTags := jsonTagsAllLevels(t)
	for _, tag := range topTags {
		if _, ok := got[tag]; !ok {
			t.Errorf("生成 JSON 缺顶层字段 %q（Config 全集对拍）", tag)
		}
	}
	if _, ok := got["_doc"]; !ok {
		t.Error("生成 JSON 缺顶层 _doc 说明字段")
	}
	// 各段：子结构 tag 全集+段级 _doc。
	for section, tags := range sectionTags {
		sub, ok := got[section].(map[string]any)
		if !ok {
			t.Fatalf("生成 JSON 段 %q 非对象", section)
		}
		for _, tag := range tags {
			if _, ok := sub[tag]; !ok {
				t.Errorf("生成 JSON 段 %q 缺字段 %q（Config 全集对拍）", section, tag)
			}
		}
		if _, ok := sub["_doc"]; !ok {
			t.Errorf("生成 JSON 段 %q 缺 _doc 说明字段", section)
		}
	}
	// 值面：剥 _doc 后与 Default() 深比较。
	defBytes, err := json.Marshal(config.Default())
	if err != nil {
		t.Fatalf("marshal Default 失败: %v", err)
	}
	var want map[string]any
	if err := json.Unmarshal(defBytes, &want); err != nil {
		t.Fatalf("unmarshal Default 失败: %v", err)
	}
	stripDocKeys(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("生成内容与 config.Default() 漂移:\n got=%v\nwant=%v", got, want)
	}
}

// TestConfigInitIdempotent 幂等态①：已存在不覆盖——退 1（对齐 backup 拒覆盖
// 先例 B6-T2）+stderr 含「已存在」+原文件内容原样（用户手改配置不被静默冲掉）。
func TestConfigInitIdempotent(t *testing.T) {
	dir := t.TempDir()
	if code, _, _ := runConfigCmd(t, dir, "init"); code != 0 {
		t.Fatalf("首次生成退码 = %d，期望 0", code)
	}
	path := filepath.Join(dir, "aiteam-config.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读首生成文件失败: %v", err)
	}
	code, _, stderr := runConfigCmd(t, dir, "init")
	if code != 1 {
		t.Fatalf("重跑退码 = %d，期望 1（已存在拒绝覆盖，对齐 backup 先例）", code)
	}
	if !strings.Contains(stderr, "已存在") {
		t.Errorf("stderr 缺「已存在」提示: %q", stderr)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("复读文件失败: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("已存在时文件被改动（应原样保留用户手改内容）")
	}
}

// TestConfigInitForce 幂等态②：--force 覆盖——篡改文件后 --force 重跑退 0，
// 内容恢复默认模板（默认值在位）。
func TestConfigInitForce(t *testing.T) {
	dir := t.TempDir()
	if code, _, _ := runConfigCmd(t, dir, "init"); code != 0 {
		t.Fatalf("首次生成退码 = %d，期望 0", code)
	}
	path := filepath.Join(dir, "aiteam-config.json")
	if err := os.WriteFile(path, []byte(`{"listen":"9.9.9.9:1"}`), 0o644); err != nil {
		t.Fatalf("篡改文件失败: %v", err)
	}
	code, stdout, _ := runConfigCmd(t, dir, "init", "--force")
	if code != 0 {
		t.Fatalf("--force 退码 = %d，期望 0", code)
	}
	if !strings.Contains(stdout, "重启 serve 生效") {
		t.Errorf("--force 后 stdout 缺提示: %q", stdout)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("复读文件失败: %v", err)
	}
	if !strings.Contains(string(data), "0.0.0.0:8310") {
		t.Errorf("--force 后内容未恢复默认模板: %s", data)
	}
}

// TestConfigInitGeneratedFileLoads 闭环验证：生成的文件直接喂 config.Load，
// _doc 未知字段按惯例忽略，加载值=Default()（生成物可被 serve 直接消费——
// 「拷贝即跑」体验的机械证明）。
func TestConfigInitGeneratedFileLoads(t *testing.T) {
	dir := t.TempDir()
	if code, _, _ := runConfigCmd(t, dir, "init"); code != 0 {
		t.Fatalf("生成退码 = %d，期望 0", code)
	}
	cfg, err := config.Load(filepath.Join(dir, "aiteam-config.json"))
	if err != nil {
		t.Fatalf("config.Load 生成文件失败: %v（_doc 应按惯例忽略）", err)
	}
	if !reflect.DeepEqual(cfg, config.Default()) {
		t.Errorf("加载配置与 Default() 不等:\n got=%+v\nwant=%+v", cfg, config.Default())
	}
}

// TestConfigUsage 子命令面：config 缺子命令/未知子命令 → ErrUsage（main 层
// 退 2），错误信息含 init 提示。
func TestConfigUsage(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{nil, {"bogus"}} {
		code, _, stderr := runConfigCmd(t, dir, args...)
		if code != 2 {
			t.Errorf("args=%v 退码 = %d，期望 2（用法错误）", args, code)
		}
		if !strings.Contains(stderr, "init") {
			t.Errorf("args=%v 错误信息缺 init 提示: %q", args, stderr)
		}
	}
}
