package main

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agchat/internal/host"
	"agchat/internal/invite"
	"agchat/internal/oplog"
)

// ---- room ------------------------------------------------------------------

func cmdRoom(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: agchat room new|status|close|resume ...")
		return exitUsage
	}
	switch args[0] {
	case "new":
		return cmdRoomNew(args[1:])
	case "list":
		// 等价于不带 --ref 的 status：本机所有房间一行一个。
		return cmdRoomStatus(args[1:])
	case "status":
		return cmdRoomStatus(args[1:])
	case "close":
		return cmdRoomClose(args[1:])
	case "resume":
		return cmdRoomResume(args[1:])
	}
	fmt.Fprintf(os.Stderr, "未知 room 子命令: %s\n", args[0])
	return exitUsage
}

func cmdRoomNew(args []string) int {
	fs := newFlags("room new")
	topic := fs.String("topic", "", "会议议题（必填）")
	rules := fs.String("rules", "", "会议规则（可选）")
	seats := fs.Int("seats", 3, "座位数")
	rounds := fs.Int("rounds", 6, "轮数")
	minutes := fs.Int("minutes", 30, "时长上限（分钟）")
	addr := fs.String("addr", "127.0.0.1:0", "宿主监听地址")
	expose := fs.Bool("expose-lan", false, "开放内网（0.0.0.0）并把内网地址写进卡")
	db := fs.String("db", "", "数据库路径（默认 <home>/agchat.db）")
	jsonOut := fs.Bool("json", false, "stdout 只输出一行 JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if strings.TrimSpace(*topic) == "" {
		return fail("缺 --topic：--topic：会议必须有议题")
	}
	cfg := loadCfg()
	if *db != "" {
		cfg.DBPath = *db
	}
	haddr, err := ensureHost(cfg, *addr, *expose)
	if err != nil {
		return fail("%v", err)
	}
	c, err := ctl(cfg, haddr)
	if err != nil {
		return fail("%v", err)
	}
	var res host.CreateResp
	if err := c.do("POST", "/v1/room", host.CreateReq{
		Topic: *topic, Rules: *rules, Seats: *seats, Rounds: *rounds, Minutes: *minutes,
	}, &res); err != nil {
		return fail("建会失败: %v", err)
	}
	// 建会顺手发一张覆盖全部座位的卡：AC1/AC8 要求"建完会就能把卡发出去"。
	// 想再发一张（更少座位/更早的 joinBy）用 `agchat invite`。
	var card host.IssueResp
	cardOK := true
	if err := c.do("POST", "/v1/invite", host.IssueReq{Ref: res.Ref, Uses: res.Seats}, &card); err != nil {
		cardOK = false
		fmt.Fprintf(os.Stderr, "提示：自动发卡失败（%v），请手动跑 agchat invite --ref %s\n", err, res.Ref)
	}
	if *jsonOut {
		out := map[string]any{
			"ref": res.Ref, "roomRef": res.Ref, "roomId": res.RoomID, "topic": res.Topic, "roomNo": res.RoomNo,
			"seats": res.Seats, "maxRounds": res.MaxRounds, "minutes": res.Minutes,
			"estimate": res.Estimate, "addr": res.Addr, "hostFile": res.HostFile, "db": res.DB,
		}
		if cardOK {
			out["card"] = card.Card
			out["human"] = card.Human
			out["machine"] = card.Machine
		}
		printJSON(out)
		return exitOK
	}
	fmt.Printf("roomRef: %s\n", res.Ref)
	fmt.Printf("roomNo: %s   topic: %s\n", res.RoomNo, res.Topic)
	fmt.Printf("seats: %d   maxRounds: %d   minutes: %d\n", res.Seats, res.MaxRounds, res.Minutes)
	fmt.Printf("estimate: 约 %d est_tokens（low %d / high %d，fits=%v）\n",
		res.Estimate.Tokens, res.Estimate.Low, res.Estimate.High, res.Estimate.Fits)
	fmt.Printf("addr: %s\n", res.Addr)
	fmt.Printf("host: %s\n", res.HostFile)
	if cardOK {
		fmt.Println()
		fmt.Println("=== 邀请卡（整段转发；人读块可改，机读块一个字都不能动。===")
		fmt.Println(card.Card)
		fmt.Println("=== 卡结束 ===")
		fmt.Println()
	}
	fmt.Printf("本机常驻，关掉终端不影响；重启电脑后需 `agchat room resume --ref %s`\n", res.Ref)
	return exitOK
}

func hostOrDead(cfg host.Config, ref string) (string, int) {
	r, err := host.ReadRendezvous(cfg, ref)
	if err != nil {
		return "", fail("这个 home 里没有房间 %s（先 agchat room new）", ref)
	}
	if !healthz(r.Addr) {
		return "", fail("宿主没在跑（pid=%d addr=%s）。用 `agchat room resume --ref %s` 把它叫回来）", r.PID, r.PID, r.Addr, ref)
	}
	return r.Addr, exitOK
}

func cmdRoomStatus(args []string) int {
	fs := newFlags("room status")
	ref := fs.String("ref", "", "房间 ref（省略 = 本 home 全部房间）")
	jsonOut := fs.Bool("json", false, "stdout 只输出一行 JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg := loadCfg()
	var haddr string
	if strings.TrimSpace(*ref) == "" {
		r, err := host.ReadHomeRendezvous(cfg)
		if err != nil || !healthz(r.Addr) {
			return fail("本机没有在跑的宿主（room new 会起一个；重启过机器的话用 room resume --ref <ref>）")
		}
		haddr = r.Addr
	} else {
		// 先分清"这是别人的会"这种情况：B 从没建过会，若它在本机有座位凭据，
		// 说明宿主在发起人机器上——别让 B 以为自己操作错了。
		rd, rdErr := host.ReadRendezvous(cfg, *ref)
		if (rdErr != nil || !healthz(rd.Addr)) && fileExists(cfg.SeatPath(*ref)) {
			fmt.Fprintln(os.Stderr, "本机没有这场会的宿主（它跑在发起人机器上）；看进展请用 agchat next / agchat result")
			return exitFail
		}
		var code int
		haddr, code = hostOrDead(cfg, *ref)
		if code != exitOK {
			return code
		}
	}
	c, err := ctl(cfg, haddr)
	if err != nil {
		return fail("%v", err)
	}
	if strings.TrimSpace(*ref) == "" {
		var res struct {
			Rooms []*host.StatusResp `json:"rooms"`
			PID   int                `json:"pid"`
			Addr  string             `json:"addr"`
		}
		if err := c.do("GET", "/v1/rooms", nil, &res); err != nil {
			return fail("读状态失败: %v", err)
		}
		if *jsonOut {
			printJSON(res)
			return exitOK
		}
		if len(res.Rooms) == 0 {
			fmt.Println("这个 home 里还没有房间")
			return exitOK
		}
		for _, st := range res.Rooms {
			printStatusLine(st)
		}
		return exitOK
	}
	var st host.StatusResp
	if err := c.do("GET", "/v1/status?ref="+*ref, nil, &st); err != nil {
		return fail("读状态失败: %v", err)
	}
	if *jsonOut {
		printJSON(st)
		return exitOK
	}
	printStatusLine(&st)
	return exitOK
}

func printStatusLine(st *host.StatusResp) {
	fmt.Printf("ref: %s  roomNo: %s  state: %s  round: %d/%d\n", st.Ref, st.RoomNo, st.State, st.Round, st.MaxRounds)
	fmt.Printf("  topic: %s\n", st.Topic)
	fmt.Printf("  seats: %d  joined: %d  messages: %d  proposals: %d  votes: %d\n",
		st.Seats, st.Joined, st.Messages, st.Proposals, st.Votes)
	fmt.Printf("  budget: used %d / %d（剩余 %d）\n", st.Budget["used"], st.Budget["tokenBudget"], st.Budget["remaining"])
	for _, m := range st.Members {
		fmt.Printf("  - %s (%s) voted=%v\n", m.Alias, m.AgentID, m.Voted)
	}
	if st.EndReason != "" {
		fmt.Printf("  endReason: %s\n", st.EndReason)
	}
}

func cmdRoomClose(args []string) int {
	fs := newFlags("room close")
	ref := fs.String("ref", "", "房间 ref")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg := loadCfg()
	r, err := resolveRef(cfg, *ref)
	if err != nil {
		return fail("%v", err)
	}
	haddr, code := hostOrDead(cfg, r)
	if code != exitOK {
		return code
	}
	c, err := ctl(cfg, haddr)
	if err != nil {
		return fail("%v", err)
	}
	var st host.StatusResp
	if err := c.do("POST", "/v1/close", map[string]string{"ref": r}, &st); err != nil {
		return fail("关会失败: %v", err)
	}
	fmt.Printf("state: %s  endReason: %s  ref: %s\n", st.State, st.EndReason, st.Ref)
	return exitOK
}

func cmdRoomResume(args []string) int {
	fs := newFlags("room resume")
	ref := fs.String("ref", "", "房间 ref")
	addr := fs.String("addr", "", "覆盖监听地址（默认沿用 host.json 里的 addr）")
	expose := fs.Bool("expose-lan", false, "开放内网")
	jsonOut := fs.Bool("json", false, "stdout 只输出一行 JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg := loadCfg()
	r, err := resolveRef(cfg, *ref)
	if err != nil {
		return fail("%v", err)
	}
	// 沿用 host.json 里的 addr：端口换了卡片就失效了。
	want := *addr
	if want == "" {
		if rd, err := host.ReadRendezvous(cfg, r); err == nil && rd.Addr != "" {
			want = rd.Addr
		}
	}
	haddr, err := ensureHost(cfg, want, *expose)
	if err != nil {
		return fail("%v", err)
	}
	c, err := ctl(cfg, haddr)
	if err != nil {
		return fail("%v", err)
	}
	var res map[string]any
	if err := c.do("POST", "/v1/resume", map[string]string{"ref": r}, &res); err != nil {
		return fail("恢复失败: %v", err)
	}
	if *jsonOut {
		res["ref"] = r
		res["addr"] = haddr
		printJSON(res)
		return exitOK
	}
	fmt.Printf("宿主已就绪  pid= pid=%v  addr=%s  ref=%s\n", res["pid"], haddr, r)
	fmt.Printf("座位凭据继续有效：%s\ns\n", cfg.SeatPath(r))
	return exitOK
}

// ---- invite ----------------------------------------------------------------

func cmdInvite(args []string) int {
	fs := newFlags("invite")
	ref := fs.String("ref", "", "房间 ref")
	uses := fs.Int("uses", 1, "这张卡的可进场人数")
	exp := fs.Int64("exp", 0, "joinBy（毫秒时间戳；0 = 不过期） = 不过期）")
	jsonOut := fs.Bool("json", false, "stdout 只输出一行 JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg := loadCfg()
	r, err := resolveRef(cfg, *ref)
	if err != nil {
		return fail("%v", err)
	}
	haddr, code := hostOrDead(cfg, r)
	if code != exitOK {
		return code
	}
	c, err := ctl(cfg, haddr)
	if err != nil {
		return fail("%v", err)
	}
	var res host.IssueResp
	if err := c.do("POST", "/v1/invite", host.IssueReq{Ref: r, Uses: *uses, Exp: *exp}, &res); err != nil {
		return fail("发卡失败: %v", err)
	}
	if *jsonOut {
		printJSON(res)
		return exitOK
	}
	if rd, err := host.ReadRendezvous(cfg, r); err == nil {
		fmt.Fprintf(os.Stderr, "提示：宿主监听 %s（默认只绑 loopback，未开放内网时会话只在本机可用）。\n", rd.Addr)
	}
	fmt.Println(res.Card)
	return exitOK
}

// ---- join ------------------------------------------------------------------

func cmdJoin(args []string) int {
	fs := newFlags("join")
	card := fs.String("card", "", "卡文件路径或整段卡文本")
	as := fs.String("as", "", "展示名（alias；不是授权依据）")
	model := fs.String("model", "", "模型名（可选）")
	expectRef := fs.String("ref", "", "会话/房间 ref：给了且与卡里不一致 → 第三类失败「这是另一场会的卡」")
	yes := fs.Bool("yes", false, "确认进场（不加只会打印同意门信息）")
	jsonOut := fs.Bool("json", false, "stdout 只输出一行 JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg := loadCfg()
	// 只给 --ref：认回自己在某场会里的座位（卡可以不在手上）。
	//
	// 场景：agent 的沙箱被换掉，token 没了，但座位私钥还在这台机器上。
	// 这时候回头找主人要卡是最贵的一步，而私钥已经足够证明"我是原来那座"。
	if strings.TrimSpace(*card) == "" && strings.TrimSpace(*expectRef) != "" {
		return reclaimByRef(cfg, strings.TrimSpace(*expectRef), *as, *model, *jsonOut)
	}
	text, ok := joinCardFromArg(*card)
	if !ok {
		return fail("缺 --card：需要卡文件路径或整段卡文本（认回旧座位可改用 --ref）")
	}
	crd, err := invite.ParseAt(text, nowMS())
	if err != nil {
		return fail("%s", invite.HumanError(err))
	}
	// 过期只在本地**提示**：exp 是没签名的字段，宿主验签后才是权威判定（A17/A3）。
	if err := invite.CheckJoinBy(crd); err != nil {
		fmt.Fprintf(os.Stderr, "提示：%s（以宿主判定为准）\n", invite.HumanError(err))
	}
	// 第三类失败（docs/09 §2.4）：验签能过，但 ref 与当前会话不符。
	if strings.TrimSpace(*expectRef) != "" && *expectRef != crd.P.Ref {
		fmt.Fprintln(os.Stderr, invite.HumanWrongRoom)
		return exitWrongRoom
	}
	endpoint := cardEndpoint(crd)
	if endpoint == "" {
		return fail("这张卡没有可用端点（ep 为空），只能让房主用 CLI 本地桥")
	}

	// 已经在本场会里且座位凭据还有效：直接报个到，不重复进场。
	if sc, err := loadSeat(cfg, crd.P.Ref); err == nil && sc.Token != "" {
		c := newClient(sc.Endpoint, sc.Token)
		var hr headResp
		if err := c.do("GET", "/v1/head", nil, &hr); err == nil {
			if *jsonOut {
				printJSON(map[string]any{"joined": true, "status": "already_joined", "ref": sc.Ref,
					"agentId": sc.AgentID, "keyFile": cfg.SeatPath(sc.Ref),
					"receiptsFile": cfg.ReceiptsPath(sc.Ref)})
			} else {
				fmt.Printf("已经在本场会里：agentId=%s alias=%s\n", sc.AgentID, sc.Alias)
				fmt.Printf("座位凭据: %s\n", cfg.SeatPath(sc.Ref))
			}
			return exitOK
		}
	}

	var pub ed25519.PublicKey
	var priv ed25519.PrivateKey
	if sc, err := loadSeat(cfg, crd.P.Ref); err == nil {
		if p, err := b64.DecodeString(sc.AgentPriv); err == nil && len(p) == ed25519.PrivateKeySize {
			priv = ed25519.PrivateKey(p)
			pub = priv.Public().(ed25519.PublicKey)
		}
	}
	if priv == nil {
		p, k, err := newAgentKey()
		if err != nil {
			return fail("生成 agentKey 失败: %v", err)
		}
		pub, priv = p, k
	}
	nonceBytes := make([]byte, invite.NonceBytes)
	if _, err := rand.Read(nonceBytes); err != nil {
		return fail("生成 nonce 失败: %v", err)
	}
	nonce := b64.EncodeToString(nonceBytes)

	c := newClient(endpoint, "")
	var jr host.JoinResp
	proof := b64.EncodeToString(ed25519.Sign(priv, host.JoinProofMessage(text, nonce)))
	if err := c.do("POST", "/v1/join", host.JoinReq{
		Card: text, AgentPub: b64.EncodeToString(pub), Nonce: nonce, Proof: proof,
		Alias: *as, Model: *model, DryRun: !*yes,
	}, &jr); err != nil {
		return fail("%v", err)
	}

	if !*yes {
		cs := jr.Consent
		if cs == nil {
			return fail("宿主没有返回同意门信息（版本不匹配？？）")
		}
		if *jsonOut {
			printJSON(map[string]any{
				"joined": false, "status": "consent_required", "ref": cs.Ref,
				"hostName": cs.HostName, "owner": cs.Owner, "topic": cs.Topic,
				"seats": cs.Seats, "joinedCount": cs.Joined, "rounds": cs.Rounds,
				"minutes": cs.Minutes, "joinBy": cs.JoinBy, "tokenBudget": cs.TokenBudget,
				"estimate": cs.Estimate, "caps": cs.Caps,
			})
			return exitConsent
		}
		fmt.Printf("谁开的：%s（宿主 %s）\n", cs.Owner, cs.HostName)
		fmt.Printf("议什么：%s\n", cs.Topic)
		fmt.Printf("预计花多少：约 %d est_tokens（区间 %d–%d，预算上限 %d）\n",
			cs.Estimate["tokens"], cs.Estimate["low"], cs.Estimate["high"], cs.TokenBudget)
		fmt.Printf("几人：%d（已进 %d）· 轮次：%d · 时长上限：%d 分钟\n", cs.Seats, cs.Joined, cs.Rounds, cs.Minutes)
		if cs.JoinBy > 0 {
			fmt.Printf("进场截止：%s\n", time.UnixMilli(cs.JoinBy).Local().Format("2006-01-02 15:04"))
		}
		fmt.Println("未进场（加 --yes 才真正加入）")
		return exitConsent
	}

	if jr.AgentID == "" {
		return fail("宿主没有返回座位")
	}
	ref := crd.P.Ref
	sc := &seatCred{
		V: 1, Ref: ref, AgentID: jr.AgentID, Alias: jr.Alias, Token: jr.Token,
		AgentPub: b64.EncodeToString(pub), AgentPriv: b64.EncodeToString(priv),
		Caps: jr.Caps, Endpoint: endpoint, JoinedAt: nowMS(),
	}
	if err := saveSeat(cfg, sc); err != nil {
		return fail("写座位凭据失败: %v", err)
	}
	if *jsonOut {
		printJSON(map[string]any{
			"joined": true, "ref": sc.Ref, "agentId": sc.AgentID, "alias": sc.Alias,
			"keyFile": cfg.SeatPath(sc.Ref), "receiptsFile": cfg.ReceiptsPath(sc.Ref),
			"isModerator": jr.IsModerator, "state": jr.State, "reclaimed": jr.Reclaimed,
		})
		return exitOK
	}
	if jr.Reclaimed {
		fmt.Printf("已认回原座位  ref: %s  agentId: %s  alias: %s（同一个座位，不占新的）\n", sc.Ref, sc.AgentID, sc.Alias)
		fmt.Printf("座位凭据已更新: %s\n", cfg.SeatPath(sc.Ref))
		fmt.Printf("下一步: agchat next agchat next --ref %s\n", sc.Ref)
		return exitOK
	}
	fmt.Printf("已进场  ref: %s  agentId: %s  alias: %s\n", sc.Ref, sc.AgentID, sc.Alias)
	fmt.Printf("座位凭据: %s（私钥只在本机）\n", cfg.SeatPath(sc.Ref))
	fmt.Printf("下一步: agchat next --ref %s\n", sc.Ref)
	return exitOK
}

// reclaimByRef 只凭本机保存的座位私钥认回座位（join --ref 且不给卡时）。
//
// 宿主要在 /v1/join 里用 agentPub 找到原座位；找不到就 404 not_seated ——那是
// "这副私钥与会无关"，与"卡不对"不是一回事，所以单独给一句话。
func reclaimByRef(cfg host.Config, ref, as, model string, jsonOut bool) int {
	sc, err := loadSeat(cfg, ref)
	if err != nil {
		return fail("本机没有这场会的座位凭据（%s）；首次进场请用 --card", cfg.SeatPath(ref))
	}
	priv, err := b64.DecodeString(sc.AgentPriv)
	if err != nil || len(priv) != ed25519.PrivateKeySize {
		return fail("座位凭据里的私钥坏了：%s", cfg.SeatPath(ref))
	}
	nonceBytes := make([]byte, invite.NonceBytes)
	if _, err := rand.Read(nonceBytes); err != nil {
		return fail("生成 nonce 失败: %v", err)
	}
	c := newClient(sc.Endpoint, "")
	var res host.JoinResp
	proof := b64.EncodeToString(ed25519.Sign(ed25519.PrivateKey(priv), host.JoinProofMessage(ref, b64.EncodeToString(nonceBytes))))
	err = c.do("POST", "/v1/join", host.JoinReq{
		Ref: ref, AgentPub: sc.AgentPub, Nonce: b64.EncodeToString(nonceBytes), Proof: proof,
		Alias: firstNonEmpty(as, sc.Alias), Model: model,
	}, &res)
	if err != nil {
		if strings.Contains(err.Error(), "not_seated") {
			return fail("这副私钥在这场会里没有座位（座位凭据可能来自另一场会）；首次进场请用 --card")
		}
		return fail("认回失败: %v", err)
	}
	sc.Token = res.Token
	sc.Alias = res.Alias
	if res.AgentID != "" {
		sc.AgentID = res.AgentID
	}
	if err := saveSeat(cfg, sc); err != nil {
		return fail("写座位凭据失败: %v", err)
	}
	if jsonOut {
		printJSON(map[string]any{
			"joined": true, "status": "reclaimed", "ref": sc.Ref, "agentId": sc.AgentID,
			"alias": sc.Alias, "keyFile": cfg.SeatPath(sc.Ref), "reclaimed": true,
		})
		return exitOK
	}
	fmt.Printf("已认回原座位  ref: %s  agentId: %s  alias: %s（同一个座位，不占新的）\n", sc.Ref, sc.AgentID, sc.Alias)
	fmt.Printf("下一步: agchat next agchat next --ref %s\n", sc.Ref)
	return exitOK
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// ---- next ------------------------------------------------------------------
func cmdNext(args []string) int {
	fs := newFlags("next")
	ref := fs.String("ref", "", "房间 ref")
	wait := fs.Int("wait", 20, "长轮询最多等多少秒（0 = 立即返回）")
	jsonOut := fs.Bool("json", false, "stdout 只输出一行 JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg := loadCfg()
	r, err := resolveRef(cfg, *ref)
	if err != nil {
		return fail("%v", err)
	}
	sc, err := loadSeat(cfg, r)
	if err != nil {
		return fail("%v", err)
	}
	c := newClient(sc.Endpoint, sc.Token)
	var res host.NextResp
	if err := c.do("GET", fmt.Sprintf("/v1/next?wait=%d", *wait), nil, &res); err != nil {
		// token 作废（宿主重启、座位被认回、租约过期）时，最有用的一句话是「怎么回来」，
		// 而不是把 401 原样抛给 agent——它只会去猜。
		if isAuthErr(err) {
			fmt.Fprintf(os.Stderr, "座位凭据已失效：%v\n", err)
			fmt.Fprintf(os.Stderr, "用这张座位自己的私钥认回（不占新座位、不用再要卡）：\n  agchat join --ref %s --yes\n", r)
			if fileExists(cfg.SeatPath(r)) {
				return exitAuth
			}
		}
		return fail("%v", err)
	}
	if *jsonOut {
		res.Ref = r
		printJSON(res)
		return exitOK
	}
	deadline := "-"
	if res.YourTurnDeadlineInSec != nil {
		deadline = fmt.Sprintf("%d", *res.YourTurnDeadlineInSec)
	}
	fmt.Printf("state: %s  round: %d/%d  yourTurn: %v  deadlineInSec: %s  remaining: %d\n",
		res.State, res.Round, res.RoundsTotal, res.YourTurn, deadline, res.Budget["remaining"])
	if res.YouAreWinner {
		fmt.Println("youAreWinner: true（该你写概述了：agchat summary --ref " + r + " --text ......）")
	}
	for _, n := range res.Notes {
		fmt.Printf("note: %s\n", n)
	}
	return exitOK
}

// ---- 参与者写操作 ----------------------------------------------------------

type opEnvelope struct {
	Op    oplog.Op `json:"op"`
	OpKey string   `json:"opKey,omitempty"`
}

type opResp struct {
	OK        bool            `json:"ok"`
	Result    json.RawMessage `json:"result"`
	Head      host.HeadView   `json:"head"`
	Ack       *oplog.Ack      `json:"ack"`
	Duplicate bool            `json:"duplicate"`
}

// sendOp 是 CLI 侧唯一"签名 → 提交"入口。
//
// 流程严格按 oplog 的约定：先 GET /v1/head 取 i/ph → 作者自己签 → 宿主只验签。
// 若链头在签名后变了（409 head_conflict），刷新链头重签一次（这是唯一的重试）。
func sendOp(sc *seatCred, kind, path, opKey string, build func(hr headResp) map[string]any) (*opResp, error) {
	priv, err := sc.priv()
	if err != nil {
		return nil, err
	}
	c := newClient(sc.Endpoint, sc.Token)
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		var hr headResp
		if err := c.do("GET", "/v1/head", nil, &hr); err != nil {
			return nil, err
		}
		payload := build(hr)
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		op := oplog.Op{I: hr.Head.I + 1, T: nowMS(), O: kind, A: sc.AgentID, P: raw, PH: hr.Head.H}
		sealed, err := oplog.SealOp(op, priv)
		if err != nil {
			return nil, err
		}
		var resp opResp
		err = c.do("POST", path, opEnvelope{Op: sealed, OpKey: opKey}, &resp)
		if err == nil {
			appendReceipt(loadCfg(), sc.Ref, resp.Ack)
			return &resp, nil
		}
		lastErr = err
		var ae *apiError
		if !asAPIError(err, &ae) || ae.Code != "head_conflict" {
			return nil, err
		}
	}
	return nil, lastErr
}

func asAPIError(err error, out **apiError) bool {
	ae, ok := err.(*apiError)
	if ok {
		*out = ae
	}
	return ok
}

func newOpKey() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("k%d", nowMS())
	}
	return b64.EncodeToString(b)
}

func cmdPost(args []string) int {
	fs := newFlags("post")
	ref := fs.String("ref", "", "房间 ref")
	text := fs.String("text", "", "发言内容")
	replyTo := fs.Int("reply-to", 0, "回复哪一条（交锋轮必填）")
	support := fs.String("support", "", "声明支持哪个方案（收尾轮（收尾轮用）")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg := loadCfg()
	r, err := resolveRef(cfg, *ref)
	if err != nil {
		return fail("%v", err)
	}
	sc, err := loadSeat(cfg, r)
	if err != nil {
		return fail("%v", err)
	}
	resp, err := sendOp(sc, oplog.OpPost, "/v1/post", newOpKey(), func(hr headResp) map[string]any {
		p := map[string]any{
			"agentId": sc.AgentID, "alias": sc.Alias, "text": *text,
			"round": hr.Round, "seq": hr.Seq + 1,
		}
		if *replyTo > 0 {
			p["replyTo"] = *replyTo
		}
		if *support != "" {
			p["support"] = *support
		}
		return p
	})
	if err != nil {
		return fail("发言失败: %v", err)
	}
	var pr struct {
		Seq   int    `json:"seq"`
		State string `json:"state"`
	}
	_ = json.Unmarshal(resp.Result, &pr)
	fmt.Printf("seq: %d  state: %s  duplicate: %v\n", pr.Seq, pr.State, resp.Duplicate)
	if resp.Ack != nil {
		fmt.Printf("ack: i=%d h=%s\n", resp.Ack.I, resp.Ack.H)
	}
	return exitOK
}

func cmdPropose(args []string) int {
	fs := newFlags("propose")
	ref := fs.String("ref", "", "房间 ref")
	title := fs.String("title", "", "方案标题（≤60 字）")
	body := fs.String("body", "", "方案正文")
	revises := fs.String("revises", "", "修订自己已有的方案 ID")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg := loadCfg()
	r, err := resolveRef(cfg, *ref)
	if err != nil {
		return fail("%v", err)
	}
	sc, err := loadSeat(cfg, r)
	if err != nil {
		return fail("%v", err)
	}
	resp, err := sendOp(sc, oplog.OpPropose, "/v1/propose", newOpKey(), func(hr headResp) map[string]any {
		p := map[string]any{
			"agentId": sc.AgentID, "alias": sc.Alias, "title": *title, "body": *body,
			"round": hr.Round, "seq": hr.Seq + 1,
		}
		if *revises != "" {
			p["supersedes"] = *revises
		}
		return p
	})
	if err != nil {
		return fail("提交方案失败: %v", err)
	}
	var pr struct {
		ProposalID string `json:"proposalId"`
		Count      int    `json:"count"`
	}
	_ = json.Unmarshal(resp.Result, &pr)
	fmt.Printf("proposalId: %s  count: %d  duplicate: %v\n", pr.ProposalID, pr.Count, resp.Duplicate)
	return exitOK
}

func cmdVote(args []string) int {
	fs := newFlags("vote")
	ref := fs.String("ref", "", "房间 ref")
	proposal := fs.String("proposal", "", "投给哪个方案")
	abstain := fs.Bool("abstain", false, "弃权")
	reason := fs.String("reason", "", "理由（可选）")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *proposal == "" && !*abstain {
		return fail("投票必须二选一（--proposal P 或 --abstain）-proposal P （--proposal P 或 --abstain）--abstain")
	}
	if *proposal != "" && *abstain {
		return fail("--proposal （--proposal P 或 --abstain）--abstain 不能同时给）")
	}
	cfg := loadCfg()
	r, err := resolveRef(cfg, *ref)
	if err != nil {
		return fail("%v", err)
	}
	sc, err := loadSeat(cfg, r)
	if err != nil {
		return fail("%v", err)
	}
	resp, err := sendOp(sc, oplog.OpVote, "/v1/vote", newOpKey(), func(hr headResp) map[string]any {
		p := map[string]any{"agentId": sc.AgentID, "alias": sc.Alias, "round": hr.Round}
		if *proposal != "" {
			p["proposalId"] = *proposal
		}
		if *reason != "" {
			p["reason"] = *reason
		}
		return p
	})
	if err != nil {
		return fail("投票失败: %v", err)
	}
	var vr struct {
		Votes map[string]int `json:"votes"`
		Cast  int            `json:"cast"`
		State string         `json:"state"`
	}
	_ = json.Unmarshal(resp.Result, &vr)
	fmt.Printf("cast: %d  state: %s  votes: %v\n", vr.Cast, vr.State, vr.Votes)
	return exitOK
}

func cmdSummary(args []string) int {
	fs := newFlags("summary")
	ref := fs.String("ref", "", "房间 ref")
	text := fs.String("text", "", "概述正文")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if strings.TrimSpace(*text) == "" {
		return fail("缺 --text")
	}
	cfg := loadCfg()
	r, err := resolveRef(cfg, *ref)
	if err != nil {
		return fail("%v", err)
	}
	sc, err := loadSeat(cfg, r)
	if err != nil {
		return fail("%v", err)
	}
	resp, err := sendOp(sc, oplog.OpSummary, "/v1/summary", newOpKey(), func(hr headResp) map[string]any {
		return map[string]any{"agentId": sc.AgentID, "alias": sc.Alias, "text": *text}
	})
	if err != nil {
		return fail("写概述失败: %v", err)
	}
	var sr struct {
		OK     bool   `json:"ok"`
		Saved  bool   `json:"saved"`
		State  string `json:"state"`
		Author string `json:"author"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(resp.Result, &sr)
	fmt.Printf("saved: %v  state: %s  author: %s  reason: %s\n", sr.Saved, sr.State, sr.Author, sr.Reason)
	return exitOK
}

// ---- result / export -------------------------------------------------------

func cmdResult(args []string) int {
	fs := newFlags("result")
	ref := fs.String("ref", "", "房间 ref")
	jsonOut := fs.Bool("json", false, "stdout 只输出一行 JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg := loadCfg()
	r, err := resolveRef(cfg, *ref)
	if err != nil {
		return fail("%v", err)
	}
	sc, err := loadSeat(cfg, r)
	if err != nil {
		return fail("%v", err)
	}
	c := newClient(sc.Endpoint, sc.Token)
	var raw json.RawMessage
	if err := c.do("GET", "/v1/result", nil, &raw); err != nil {
		return fail("读结论失败: %v", err)
	}
	if *jsonOut {
		fmt.Println(string(raw))
		return exitOK
	}
	var res struct {
		OwnerReport    string `json:"ownerReport"`
		DecisionHash   string `json:"decisionHash"`
		TranscriptHash string `json:"transcriptHash"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fail("结论 JSON 解析失败: %v", err)
	}
	fmt.Println(res.OwnerReport)
	fmt.Printf("\ndecisionHash: %s\ntranscriptHash: %s\n", res.DecisionHash, res.TranscriptHash)
	return exitOK
}

func cmdExport(args []string) int {
	fs := newFlags("export")
	ref := fs.String("ref", "", "房间 ref")
	out := fs.String("out", "bundle.zip", "输出文件")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg := loadCfg()
	r, err := resolveRef(cfg, *ref)
	if err != nil {
		return fail("%v", err)
	}
	haddr, code := hostOrDead(cfg, r)
	if code != exitOK {
		return code
	}
	c, err := ctl(cfg, haddr)
	if err != nil {
		return fail("%v", err)
	}
	req, err := http.NewRequest("GET", c.base+"/v1/export?ref="+r, nil)
	if err != nil {
		return fail("导出失败: %v", err)
	}
	req.Header.Set("authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fail("导出失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fail("读导出内容失败: %v", err)
	}
	if resp.StatusCode >= 400 {
		return fail("导出失败（HTTP %d（HTTP %d）: %s %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		return fail("写文件失败: %v", err)
	}
	fmt.Printf("bundle: %s\n", absPath(*out))
	fmt.Printf("ops: %d\n", countOpsInBundle(data))
	return exitOK
}

func countOpsInBundle(data []byte) int {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return 0
	}
	for _, f := range zr.File {
		if filepath.Base(f.Name) != "ops.jsonl" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return 0
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return 0
		}
		n := 0
		for _, line := range bytes.Split(b, []byte("\n")) {
			if len(bytes.TrimSpace(line)) > 0 {
				n++
			}
		}
		return n
	}
	return 0
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}
