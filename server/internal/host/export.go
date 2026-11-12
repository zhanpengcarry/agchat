package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"agchat/internal/invite"
	"agchat/internal/oplog"
	"agchat/internal/room"
)

func osGetpid() int { return os.Getpid() }

// ResultResp 是 GET /v1/result 的响应：
// 房间结论（room.Result 的字段全部平铺）+ 两个顶层指纹。
//
// `decisionHash` 锚结论、`transcriptHash` 锚过程（arch C-1）。两个都必须出现，
// 否则 CLI 的 --json 契约与验收脚本就没法比对"指纹不变"。
type ResultResp struct {
	*room.Result
	DecisionHash   string `json:"decisionHash"`
	TranscriptHash string `json:"transcriptHash"`
}

// Result 返回结论。它**只读、不 tick**（arch R-7）。
func (h *Host) Result(ctx context.Context, token string) (*ResultResp, error) {
	rc, s, err := h.authByToken(token)
	if err != nil {
		return nil, err
	}
	if ok, reason := admit(s.Caps, invite.CapRead); !ok {
		return nil, newOpErr(403, "capability_denied", reason)
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return h.resultLocked(ctx, rc)
}

// ResultByRef 是控制面用（export 内部）。
func (h *Host) ResultByRef(ctx context.Context, ref string) (*ResultResp, error) {
	rc := h.roomByRef(ref)
	if rc == nil {
		return nil, errNotFound
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return h.resultLocked(ctx, rc)
}

func (h *Host) resultLocked(ctx context.Context, rc *roomCtx) (*ResultResp, error) {
	var res *room.Result
	err := h.eng.PeekReadOnly(ctx, TenantID, rc.roomID, func(agg *room.Agg) error {
		res = h.eng.ResultFor(agg)
		return nil
	})
	if err != nil {
		return nil, err
	}
	th, err := transcriptHashOf(rc)
	if err != nil {
		return nil, err
	}
	out := &ResultResp{Result: res, TranscriptHash: th}
	if res.DecisionSet != nil {
		out.DecisionHash = res.DecisionSet.Hash
	}
	// ownerReport 是给房主看的整段文字（API 层也是这么拼的）。
	out.OwnerReport = room.OwnerReportOf(res, "http://"+h.Addr()+"/v1/result")
	h.decorateReport(out.Result, rc.ref)
	return out, nil
}

// decorateReport 在 ownerReport **最前面** prepend 一行三要素头（AC3/AC8/AC11）：
// 谁开的 + 会名/议题 + 结论，再说明"这台机器上计票"。
//
// 为什么不改 room.OwnerReportOf：engine 是共享文件（agchatd 也用它），
// 宿主只在渲染出口加自己的头，agchatd 的输出保持逐字节不变。
func (h *Host) decorateReport(res *room.Result, ref string) {
	if res == nil {
		return
	}
	conclusion := strings.TrimSpace(res.Conclusion.Text)
	if conclusion == "" {
		conclusion = "未形成结论"
	}
	head := fmt.Sprintf("%s · %s 开的 · 结论：%s", res.Topic, h.cfg.OwnerName, conclusion)
	tail := fmt.Sprintf("本场会议在「%s」这台机器上计票（roomRef %s）。", h.cfg.DisplayName, invite.ShortRef(ref))
	res.OwnerReport = head + "\n" + tail + "\n\n" + res.OwnerReport
}

// transcriptHashOf 用**已提交 op 流**算转写稿指纹（不回查消息表）：
// 只取 post op 的载荷 {seq,round,alias,text,replyTo,support}（arch B-13 / C-1）。
func transcriptHashOf(rc *roomCtx) (string, error) {
	msgs := []oplog.TranscriptMsg{}
	for _, op := range rc.log.committedOps() {
		if op.O != oplog.OpPost {
			continue
		}
		var p struct {
			Seq     int    `json:"seq"`
			Round   int    `json:"round"`
			Alias   string `json:"alias"`
			Text    string `json:"text"`
			ReplyTo *int   `json:"replyTo,omitempty"`
			Support string `json:"support,omitempty"`
		}
		if err := json.Unmarshal(op.P, &p); err != nil {
			return "", err
		}
		msgs = append(msgs, oplog.TranscriptMsg{
			I: op.I, Seq: p.Seq, Alias: p.Alias, Round: p.Round,
			Text: p.Text, ReplyTo: p.ReplyTo, Support: p.Support,
		})
	}
	return oplog.TranscriptHash(msgs)
}

// Export 打出 bundle.zip（docs/09 §6 / arch C-3）：
// manifest.json + ops.jsonl（已提交前缀）+ acks.jsonl + result.json + verify-target.json。
func (h *Host) Export(ctx context.Context, ref string) ([]byte, error) {
	rc := h.roomByRef(ref)
	if rc == nil {
		return nil, errNotFound
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()

	var r *room.Result
	var topic string
	var seats, maxRounds, maxAgents, maxMinutes, maxOutput int
	err := h.eng.PeekReadOnly(ctx, TenantID, rc.roomID, func(agg *room.Agg) error {
		r0 := agg.Room
		topic, seats, maxRounds = r0.Topic, r0.MaxAgents, r0.MaxRounds
		maxAgents, maxMinutes, maxOutput = r0.MaxAgents, r0.MaxMinutes, r0.MaxOutputTokens
		r = h.eng.ResultFor(agg)
		return nil
	})
	if err != nil {
		return nil, err
	}
	th, err := transcriptHashOf(rc)
	if err != nil {
		return nil, err
	}
	dh := ""
	if r.DecisionSet != nil {
		dh = r.DecisionSet.Hash
	}
	// bundle 里的 result.json 与 /v1/result 同形（含 ownerReport 的三要素头）。
	r.OwnerReport = room.OwnerReportOf(r, "http://"+h.Addr()+"/v1/result")
	h.decorateReport(r, rc.ref)
	resultJSON, err := json.Marshal(&ResultResp{Result: r, DecisionHash: dh, TranscriptHash: th})
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(rc.inv))
	for id := range rc.inv {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	ops := rc.log.committedOps()
	acks := rc.log.acksCopy()
	b := oplog.Bundle{
		Manifest: oplog.Manifest{
			Ref: rc.ref, Protocol: oplog.Protocol,
			ResultSchema: room.ResultSchemaVersion, VoteRule: "1", HashDomain: "1",
			Seats: seats, InviteIDs: ids,
			MaxRounds: maxRounds, MaxAgents: maxAgents, MaxMinutes: maxMinutes, MaxOutput: maxOutput,
			Topic: topic, OpsVersion: 1, BundleVersion: 1,
			Expect: oplog.Expect{DecisionHash: dh, TranscriptHash: th},
		},
		Ops: ops, Acks: acks, Result: json.RawMessage(resultJSON),
		DecisionHash: dh, TranscriptHash: th,
	}
	var buf bytes.Buffer
	if err := oplog.WriteBundle(&buf, b); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// 让 "errors" 在只用一个分支时也保持导入稳定（见 New 的错误判断）。
var _ = errors.Is
