package oplog

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// 证据包内的文件名（docs/09 §6 / arch §4.4）。
const (
	FileManifest = "manifest.json"
	FileOps      = "ops.jsonl"
	FileAcks     = "acks.jsonl"
	FileResult   = "result.json"
	FileTarget   = "verify-target.json"
)

// 资源上限：证据包是本地文件，但读别人的 zip 必须防"打包炸弹"与意外大文件。
const (
	maxBundleBytes = 64 << 20 // 整个 zip 上限 64 MiB
	maxEntryBytes  = 32 << 20 // 单个 entry 上限 32 MiB
	maxLineBytes   = 8 << 20  // jsonl 单行上限 8 MiB
)

// VerifyTarget 是**声明值**（写进 verify-target.json）：verify 只拿它做差异定位，
// 真判决必须自己重算（§10.2④）。
type VerifyTarget struct {
	Ref            string `json:"ref"`
	DecisionHash   string `json:"decisionHash"`
	TranscriptHash string `json:"transcriptHash"`
}

// WriteBundle 写 zip：manifest.json / ops.jsonl / acks.jsonl / result.json / verify-target.json。
//
// 约定：
//   - ops.jsonl 每行一个规范编码的 Op（键升序、无空格、无 null、无浮点）；
//   - manifest 的 expect 与 verify-target.json 写同一份声明值（两处不一致的包，ReadBundle 会拒）；
//   - zip 内时间戳固定，同样的内容写出同样的字节（便于测试与比对）。
func WriteBundle(w io.Writer, b Bundle) error {
	if w == nil {
		return fmt.Errorf("%w: nil writer", ErrBadBundle)
	}
	man, expect, err := prepareManifest(b)
	if err != nil {
		return err
	}
	manRaw, err := CanonicalJSON(man)
	if err != nil {
		return fmt.Errorf("%w: manifest: %v", ErrBadBundle, err)
	}
	opsRaw, err := encodeOps(b.Ops)
	if err != nil {
		return err
	}
	acksRaw, err := encodeAcks(b.Acks)
	if err != nil {
		return err
	}
	targetRaw, err := CanonicalJSON(VerifyTarget{
		Ref:            man.Ref,
		DecisionHash:   expect.DecisionHash,
		TranscriptHash: expect.TranscriptHash,
	})
	if err != nil {
		return fmt.Errorf("%w: verify-target: %v", ErrBadBundle, err)
	}

	zw := zip.NewWriter(w)
	// 固定时间戳：同样的证据包写出同样的字节（zip 默认会写当前时间）。
	mtime := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	entries := []zipEntry{
		{FileManifest, manRaw},
		{FileOps, opsRaw},
		{FileAcks, acksRaw},
		{FileTarget, targetRaw},
	}
	if len(b.Result) > 0 {
		if !json.Valid(b.Result) {
			return fmt.Errorf("%w: result.json 不是合法 JSON", ErrBadBundle)
		}
		entries = append(entries, zipEntry{FileResult, b.Result})
	}
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		hdr.Modified = mtime
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			_ = zw.Close()
			return fmt.Errorf("%w: 写 %s: %v", ErrBadBundle, e.name, err)
		}
		if _, err := fw.Write(e.data); err != nil {
			_ = zw.Close()
			return fmt.Errorf("%w: 写 %s: %v", ErrBadBundle, e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("%w: 收尾 zip: %v", ErrBadBundle, err)
	}
	return nil
}

// prepareManifest 补默认值并校验 manifest，返回写入用的副本与声明值。
func prepareManifest(b Bundle) (Manifest, Expect, error) {
	man := b.Manifest
	if strings.TrimSpace(man.Ref) == "" {
		return man, Expect{}, fmt.Errorf("%w: manifest.roomRef 为空", ErrBadBundle)
	}
	if strings.TrimSpace(man.ResultSchema) == "" || strings.TrimSpace(man.VoteRule) == "" {
		// arch C-3：规则版本是证据链里唯一真正必须的元数据。缺了它，"重算不符"
		// 永远有两种解释（被篡改 / 代码升级了），verify 就失去判决力。
		return man, Expect{}, fmt.Errorf("%w: manifest 缺 resultSchema / voteRule（C-3：必须钉住规则版本）", ErrBadBundle)
	}
	if man.Seats < 1 {
		return man, Expect{}, fmt.Errorf("%w: manifest.seats=%d", ErrBadBundle, man.Seats)
	}
	if len(man.InviteIDs) == 0 {
		return man, Expect{}, fmt.Errorf("%w: manifest.inviteIds 为空（genesis 必须列全 inviteId）", ErrBadBundle)
	}
	for i, id := range man.InviteIDs {
		if strings.TrimSpace(id) == "" {
			return man, Expect{}, fmt.Errorf("%w: manifest.inviteIds[%d] 为空", ErrBadBundle, i)
		}
	}
	if strings.TrimSpace(man.Protocol) == "" {
		man.Protocol = Protocol
	}
	if strings.TrimSpace(man.HashDomain) == "" {
		man.HashDomain = HashDomain
	}
	if man.OpsVersion == 0 {
		man.OpsVersion = OpsVersion
	}
	if man.BundleVersion == 0 {
		man.BundleVersion = BundleVersion
	}
	// 声明值：以 Bundle 上的两个指纹为准；调用方没给就沿用 manifest 里已有的。
	expect := man.Expect
	if strings.TrimSpace(b.DecisionHash) != "" || strings.TrimSpace(b.TranscriptHash) != "" {
		expect = Expect{DecisionHash: b.DecisionHash, TranscriptHash: b.TranscriptHash}
	}
	man.Expect = expect
	return man, expect, nil
}

// zipEntry 是待写入的包内文件。
type zipEntry struct {
	name string
	data []byte
}

// encodeOps 把 op 逐条规范编码成 jsonl。
func encodeOps(ops []Op) ([]byte, error) {
	var buf bytes.Buffer
	for i, op := range ops {
		raw, err := CanonicalJSON(op)
		if err != nil {
			return nil, fmt.Errorf("%w: 第 %d 条 op: %v", ErrBadBundle, i+1, err)
		}
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// encodeAcks 把收据逐条规范编码成 jsonl。
func encodeAcks(acks []Ack) ([]byte, error) {
	var buf bytes.Buffer
	for i, a := range acks {
		raw, err := CanonicalJSON(a)
		if err != nil {
			return nil, fmt.Errorf("%w: 第 %d 条 ack: %v", ErrBadBundle, i+1, err)
		}
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// ReadBundle 读 zip 并还原 Bundle。
//
// manifest.json / ops.jsonl / verify-target.json 必需；acks.jsonl / result.json 允许缺失
// （缺收据 → verify 判 unverifiable，不是读包失败）。
func ReadBundle(r io.Reader) (*Bundle, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: nil reader", ErrBadBundle)
	}
	blob, err := io.ReadAll(io.LimitReader(r, maxBundleBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: 读包: %v", ErrBadBundle, err)
	}
	if len(blob) > maxBundleBytes {
		return nil, fmt.Errorf("%w: 包超过 %d 字节上限", ErrBadBundle, maxBundleBytes)
	}
	zr, err := zip.NewReader(bytes.NewReader(blob), int64(len(blob)))
	if err != nil {
		return nil, fmt.Errorf("%w: 不是合法 zip: %v", ErrBadBundle, err)
	}
	var (
		manRaw, opsRaw, acksRaw, resultRaw, targetRaw []byte
		seenMan, seenOps, seenTarget                  bool
	)
	// 条目名按 basename 归一：`zip -r out.zip ./bundle` 与 `bundle\manifest.json`
	// 都会带前缀，只做精确匹配的话，最普通的"解压看一眼再打包"就打不开了（A11）。
	// 同名条目二次出现必须报错：不同解压器取法不同，静默覆盖等于让两份流里挑一份（A10）。
	seenName := map[string]bool{}
	for _, f := range zr.File {
		name := path.Base(strings.ReplaceAll(f.Name, `\`, "/"))
		if strings.HasSuffix(f.Name, "/") {
			continue // 目录条目
		}
		switch name {
		case FileManifest, FileOps, FileAcks, FileResult, FileTarget:
		default:
			// 未知条目忽略（向前兼容），不参与判决。
			continue
		}
		if seenName[name] {
			return nil, fmt.Errorf("%w: 同名条目出现两次：%s", ErrBadBundle, name)
		}
		seenName[name] = true
		data, err := readEntry(f)
		if err != nil {
			return nil, err
		}
		switch name {
		case FileManifest:
			manRaw, seenMan = data, true
		case FileOps:
			opsRaw, seenOps = data, true
		case FileAcks:
			acksRaw = data
		case FileResult:
			resultRaw = data
		case FileTarget:
			targetRaw, seenTarget = data, true
		}
	}
	if !seenMan || !seenOps || !seenTarget {
		return nil, fmt.Errorf("%w: 缺必需条目（manifest=%v ops=%v verify-target=%v）", ErrBadBundle, seenMan, seenOps, seenTarget)
	}

	var b Bundle
	if err := json.Unmarshal(manRaw, &b.Manifest); err != nil {
		return nil, fmt.Errorf("%w: manifest.json: %v", ErrBadBundle, err)
	}
	if v := b.Manifest.BundleVersion; v != 0 && v > BundleVersion {
		return nil, fmt.Errorf("%w: bundleVersion=%d 超过本实现支持的 %d", ErrBadBundle, v, BundleVersion)
	}
	if v := b.Manifest.OpsVersion; v != 0 && v > OpsVersion {
		return nil, fmt.Errorf("%w: opsVersion=%d 超过本实现支持的 %d", ErrBadBundle, v, OpsVersion)
	}
	ops, err := decodeOps(opsRaw)
	if err != nil {
		return nil, err
	}
	b.Ops = ops
	acks, err := decodeAcks(acksRaw)
	if err != nil {
		return nil, err
	}
	b.Acks = acks
	if len(resultRaw) > 0 {
		if !json.Valid(resultRaw) {
			return nil, fmt.Errorf("%w: result.json 不是合法 JSON", ErrBadBundle)
		}
		b.Result = resultRaw
	}
	var target VerifyTarget
	if err := json.Unmarshal(targetRaw, &target); err != nil {
		return nil, fmt.Errorf("%w: verify-target.json: %v", ErrBadBundle, err)
	}
	if target.Ref != "" && b.Manifest.Ref != "" && target.Ref != b.Manifest.Ref {
		return nil, fmt.Errorf("%w: verify-target.ref=%s 与 manifest.roomRef=%s 不一致", ErrBadBundle, target.Ref, b.Manifest.Ref)
	}
	// manifest.expect 与 verify-target.json 是同一份声明值的两份拷贝；不一致说明包被改过。
	// expect 缺了就让 verify-target 变成唯一副本，两份互相印证的白送（A12）：
	// 所以 expect 从"可选"改成必需。
	if e := b.Manifest.Expect; e.DecisionHash == "" && e.TranscriptHash == "" {
		return nil, fmt.Errorf("%w: manifest 缺 expect（两个指纹必须与 verify-target 各存一份）", ErrBadBundle)
	}
	if e := b.Manifest.Expect; e.DecisionHash != "" || e.TranscriptHash != "" {
		if e.DecisionHash != target.DecisionHash || e.TranscriptHash != target.TranscriptHash {
			return nil, fmt.Errorf("%w: manifest.expect 与 verify-target.json 不一致", ErrBadBundle)
		}
	}
	b.DecisionHash = target.DecisionHash
	b.TranscriptHash = target.TranscriptHash
	return &b, nil
}

// readEntry 带单 entry 上限地读一个 zip 条目。
func readEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: 打开 %s: %v", ErrBadBundle, f.Name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxEntryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: 读 %s: %v", ErrBadBundle, f.Name, err)
	}
	if len(data) > maxEntryBytes {
		return nil, fmt.Errorf("%w: %s 超过 %d 字节上限", ErrBadBundle, f.Name, maxEntryBytes)
	}
	return data, nil
}

// decodeOps 解析 ops.jsonl（空行跳过）。
func decodeOps(raw []byte) ([]Op, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	var ops []Op
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var op Op
		if err := json.Unmarshal([]byte(text), &op); err != nil {
			return nil, fmt.Errorf("%w: ops.jsonl 第 %d 行: %v", ErrBadBundle, line, err)
		}
		ops = append(ops, op)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: 读 ops.jsonl: %v", ErrBadBundle, err)
	}
	return ops, nil
}

// decodeAcks 解析 acks.jsonl（空行跳过）。
func decodeAcks(raw []byte) ([]Ack, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	var acks []Ack
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var a Ack
		if err := json.Unmarshal([]byte(text), &a); err != nil {
			return nil, fmt.Errorf("%w: acks.jsonl 第 %d 行: %v", ErrBadBundle, line, err)
		}
		acks = append(acks, a)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: 读 acks.jsonl: %v", ErrBadBundle, err)
	}
	return acks, nil
}

// LoadBundle 是 ReadBundle 的路径版便利函数。
func LoadBundle(path string) (*Bundle, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: 打开 %s: %v", ErrBadBundle, path, err)
	}
	defer f.Close()
	return ReadBundle(f)
}

// SaveBundle 是 WriteBundle 的路径版便利函数，原子写（同目录临时文件 + rename）。
func SaveBundle(path string, b Bundle) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".bundle-*.zip")
	if err != nil {
		return fmt.Errorf("%w: 建临时文件: %v", ErrBadBundle, err)
	}
	tmpName := tmp.Name()
	defer func() {
		// 成功路径上文件已被 rename，这里只会删掉失败残留。
		_ = os.Remove(tmpName)
	}()
	if err := WriteBundle(tmp, b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%w: fsync: %v", ErrBadBundle, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("%w: close: %v", ErrBadBundle, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("%w: rename 到 %s: %v", ErrBadBundle, path, err)
	}
	return nil
}
