// Package stress 是"长会不崩"的压测套件。
//
// 它**不在默认测试套件里**（需要 //go:build stress 标签才编译）：
// 一次全量要跑 20 场 6 人 × 10 轮长会，耗时以分钟计。
//
// 跑法：
//
//	go test ./...                      默认套件（不含本包）
//	go test -tags stress ./stress      显式跑压测
//	CGO_ENABLED=1 go test -race ./...  race detector（需要 cgo / gcc）
//
// 用构建标签而不是 t.Skip：标签的语义是"这类测试本来就不在默认套件里"，
// 文件层面可见；t.Skip 的语义是"这条测试暂时不跑"，会让人以为它坏了。
package stress
