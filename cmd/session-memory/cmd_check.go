package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"session-memory/internal/agent"
	"session-memory/internal/device"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
)

// checkReport answers "is the sync working?" in words a person can act on. It
// reads local state only (no request goes to the cloud), so it is quick and
// costs no Weiyun quota.
type checkReport struct {
	Level    string   `json:"level"`    // ok | attention | problem | not_set_up
	Headline string   `json:"headline"` // one sentence for the person
	Details  []string `json:"details"`
	Next     []string `json:"next_steps,omitempty"`
}

var checkRank = map[string]int{"ok": 0, "attention": 1, "problem": 2, "not_set_up": 3}

func (c *checkReport) raise(level string) {
	if checkRank[level] > checkRank[c.Level] {
		c.Level = level
	}
}

func (c *checkReport) detail(format string, args ...any) {
	c.Details = append(c.Details, fmt.Sprintf(format, args...))
}

func (c *checkReport) next(step string) {
	for _, n := range c.Next {
		if n == step {
			return
		}
	}
	c.Next = append(c.Next, step)
}

// jobState is what launchd says about the background job.
type jobState int

const (
	jobUnknown   jobState = iota // not macOS, or cannot tell
	jobOff                       // no job file
	jobNotLoaded                 // the file is there but launchd has not loaded it
	jobRunning
)

// backgroundJob asks launchd; tests replace it.
var backgroundJob = func() jobState {
	if runtime.GOOS != "darwin" {
		return jobUnknown
	}
	if _, err := os.Stat(autosyncPlist()); err != nil {
		return jobOff
	}
	if _, err := launchctl("print", "gui/"+strconv.Itoa(os.Getuid())+"/"+autosyncLabel); err != nil {
		return jobNotLoaded
	}
	return jobRunning
}

// ago says how long ago t was, roughly.
func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 90*time.Second:
		return "刚刚"
	case d < 90*time.Minute:
		return fmt.Sprintf("%d 分钟前", int(d.Round(time.Minute).Minutes()))
	case d < 36*time.Hour:
		return fmt.Sprintf("%d 小时前", int(d.Round(time.Hour).Hours()))
	}
	return fmt.Sprintf("%d 天前", int((d+12*time.Hour).Hours()/24))
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "dev_")
	if len(id) > 6 {
		id = id[:6]
	}
	return id
}

// buildCheck looks at everything that decides whether the sync works.
func (a *App) buildCheck(ctx context.Context) checkReport {
	rep := checkReport{Level: "ok"}
	now := a.Now()
	root := a.Cfg.StateDir()

	info, err := device.At(root).Load()
	if err != nil {
		if errcode.CodeOf(err) == errcode.NoVault {
			rep.raise("not_set_up")
			rep.Headline = "这台电脑还没有设置会话同步"
			rep.next("运行 `" + selfPath() + " setup`（安装说明里的第 1 步），它会自己连接微云并开启后台同步")
			return rep
		}
		rep.raise("problem")
		rep.Headline = "这台电脑的同步设置读不出来"
		rep.detail("错误：%v", err)
		rep.next("把上面的错误原文告诉用户；不要自己删除或改动状态目录")
		return rep
	}

	// where the vault lives
	switch {
	case a.Cfg.Transport.Weiyun != "":
		if len(a.weiyunAccounts()) == 0 {
			rep.raise("problem")
			rep.detail("微云没有接通（这台电脑上找不到微云登录）")
			rep.next("按安装说明里 NEEDS_WEIYUN 那一行处理：装微云 skill，让用户用微信/QQ 扫码授权，再运行 `" + selfPath() + " setup`")
		}
	case a.Cfg.Transport.Folder == "":
		rep.raise("problem")
		rep.detail("没有配置同步位置")
		rep.next("运行 `" + selfPath() + " setup`")
	}

	// how the last background rounds went
	round, haveRound := a.loadRound()
	idle := a.loadIdle()
	lastRun := idle.LastPoll
	if haveRound && round.At.After(lastRun) {
		lastRun = round.At
	}
	paused := 0
	accounts := a.weiyunAccounts()
	for _, ac := range accounts {
		if until, p := a.quotaPausedUntil(ac.id); p {
			paused++
			if a.loadQuota()[ac.id].Rate {
				rep.detail("微云账号 %s 刚才被请求太频繁限制了，%s 起自动恢复", ac.label, until.Format("15:04"))
			} else {
				rep.detail("微云账号 %s 今天的额度用完了，%s 起自动恢复", ac.label, until.Format("1月2日 15:04"))
			}
		}
	}
	if len(accounts) > 0 && paused == len(accounts) {
		rep.raise("attention")
		rep.detail("微云今天暂时不能用：新的对话先存在本机，恢复后会自动上传，不会丢")
	}

	switch backgroundJob() {
	case jobOff:
		rep.raise("problem")
		rep.detail("后台同步没有开启（对话只在手动同步时才会传）")
		rep.next("运行 `" + selfPath() + " autosync enable`；如果提示沙盒限制，请用户双击桌面上的「开启同步.command」")
	case jobNotLoaded:
		rep.raise("problem")
		rep.detail("后台同步的任务文件在，但系统还没有加载它")
		rep.next("请用户重新登录一次电脑，或在“终端”里运行 `" + selfPath() + " autosync enable`")
	case jobRunning:
		if lastRun.IsZero() {
			rep.detail("后台同步已开启，还没有跑过第一轮")
		} else if now.Sub(lastRun) > 30*time.Minute && !(len(accounts) > 0 && paused == len(accounts)) {
			rep.raise("attention")
			rep.detail("后台同步已开启，但上一次运行是 %s，可能卡住了", ago(now, lastRun))
			rep.next("运行 `" + selfPath() + " relay` 手动同步一次，看有没有报错")
		} else {
			rep.detail("后台同步已开启，%s检查过", ago(now, lastRun))
		}
	}
	if haveRound && !round.OK {
		since := ago(now, round.Since)
		switch round.Kind {
		case "auth":
			rep.raise("problem")
			rep.detail("微云的登录失效了（%s起一直如此），同步已暂停", since)
			rep.next("让用户重新授权微云：按微云 skill 自带的 `references/auth.md` 扫码，然后运行 `" + selfPath() + " setup`")
		case "attention":
			rep.raise("attention")
			rep.detail("最近一轮同步里有对话没能装进本机")
			if _, ok := a.loadBlocked(); !ok {
				rep.next("运行 `" + selfPath() + " relay` 看是哪几个、为什么，用白话告诉用户；不要自己执行 adopt / choose，除非用户要求")
			}
		default:
			if now.Sub(round.Since) >= time.Hour {
				rep.raise("problem")
				rep.detail("同步已连续失败 %s：%s", strings.TrimSuffix(since, "前"), round.Error)
				rep.next("如果是网络问题，等网络恢复即可；否则把上面的错误原文告诉用户")
			} else {
				rep.detail("最近一轮同步没成功（可能是网络暂时不通），会自动重试：%s", round.Error)
			}
		}
	}

	// the sessions themselves
	r, err := a.openRTBriefly(ctx)
	switch {
	case err != nil && strings.Contains(err.Error(), "another session-memory is running"):
		rep.detail("正在同步中，对话数量稍后再查看")
	case err != nil:
		rep.raise("problem")
		rep.detail("读不出本机的同步记录：%v", err)
		rep.next("把上面的错误原文告诉用户；不要自己删除状态目录")
	default:
		defer r.Close()
		a.checkSessions(ctx, &rep, r, info, now)
	}

	if rep.Level == "ok" {
		rep.Headline = "同步正常"
	} else if rep.Level == "attention" {
		rep.Headline = "同步在运行，但有需要留意的地方"
	} else if rep.Headline == "" {
		rep.Headline = "同步出了问题"
	}
	if !a.Cfg.NotifyEnabled() {
		rep.detail("桌面通知已关闭（`config set notify true` 可以打开）")
	}
	return rep
}

// checkLockWait is how long `check` waits for a sync that is running.
var checkLockWait = 3 * time.Second

// openRTBriefly opens the local records without waiting long for a running sync.
func (a *App) openRTBriefly(ctx context.Context) (*rt, error) {
	old := lockWait
	lockWait = checkLockWait
	defer func() { lockWait = old }()
	return a.openRT(ctx, false)
}

func (a *App) checkSessions(ctx context.Context, rep *checkReport, r *rt, info device.Info, now time.Time) {
	rows, err := buildRows(r, "")
	if err != nil {
		rep.raise("problem")
		rep.detail("读不出对话列表：%v", err)
		return
	}
	mine := map[string]int{}
	received, unpublished, conflicts, unsafe := 0, 0, 0, 0
	var receivedIDs []listRow
	for _, x := range rows {
		if x.State == "received" {
			received++
			receivedIDs = append(receivedIDs, x)
		} else if x.State == session.StateActive {
			mine[x.Agent]++
		}
		unpublished += x.Unpub
		if x.Conflict {
			conflicts++
		}
		if x.Unsafe {
			unsafe++
		}
	}
	var parts []string
	var agents []string
	for ag := range mine {
		agents = append(agents, ag)
	}
	sort.Strings(agents)
	for _, ag := range agents {
		parts = append(parts, fmt.Sprintf("%s %d 个", agentTitle(ag), mine[ag]))
	}
	switch {
	case len(parts) == 0:
		rep.detail("本机还没有纳入同步的对话（先在 Codex 或 WorkBuddy 里聊几句，下一轮会自动加入）")
	case unpublished == 0:
		rep.detail("本机在同步的对话：%s，都已上传", strings.Join(parts, "、"))
	default:
		rep.detail("本机在同步的对话：%s；其中 %d 处新内容等待上传（后台下一轮会传）", strings.Join(parts, "、"), unpublished)
	}
	if conflicts > 0 {
		rep.raise("attention")
		rep.detail("%d 个对话在两台电脑上各改了一份，没法自动合并，需要你决定留哪份", conflicts)
		rep.next("运行 `" + selfPath() + " relay` 看是哪几个，用白话告诉用户；不要自己执行 adopt / choose，除非用户要求")
	}
	if unsafe > 0 {
		rep.raise("attention")
		rep.detail("%d 个对话的安装被中断过，需要先检查", unsafe)
		rep.next("运行 `" + selfPath() + " relay` 看详情，把原文告诉用户")
	}
	blocked := map[string]blockedEntry{}
	if st, ok := a.loadBlocked(); ok && now.Sub(st.At) <= blockedFresh {
		blocked = st.Sessions
		a.describeBlocked(ctx, rep, r, now, blocked)
	}
	if received > 0 {
		a.describeWaiting(ctx, rep, r, receivedIDs, blocked)
	}

	db := r.Store.DB()
	var quarantined int
	if db.QueryRow(`SELECT COUNT(*) FROM quarantine`).Scan(&quarantined) == nil && quarantined > 0 {
		rep.raise("attention")
		rep.detail("%d 份收到的内容校验不通过，已被隔离（没有装进任何应用）", quarantined)
		rep.next("把这条原文告诉用户；不要删除隔离的文件")
	}

	q, err := db.Query(`SELECT device_id, MAX(seen_at) FROM manifests WHERE device_id <> ? GROUP BY device_id ORDER BY 2 DESC`, info.DeviceID)
	if err != nil {
		return
	}
	defer q.Close()
	others := 0
	for q.Next() {
		var id, seen string
		if q.Scan(&id, &seen) != nil {
			continue
		}
		others++
		if t, err := session.ParseTime(seen); err == nil {
			rep.detail("别的电脑（设备 %s）：最近一次收到它的更新是 %s", shortID(id), ago(now, t))
		} else {
			rep.detail("别的电脑（设备 %s）：已连接", shortID(id))
		}
	}
	if others == 0 {
		rep.detail("还没有看到别的电脑：在另一台电脑上也装好（用同一个微信/QQ 登录微云）后，这里会显示它")
	}
}

func newCheckCmd(a *App) *cobra.Command {
	return &cobra.Command{Use: "check", Short: "Say in plain words whether the sync is working, and what to do if it is not", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep := a.buildCheck(cmd.Context())
			if a.JSON {
				return a.printJSON(rep)
			}
			fmt.Fprintln(a.Out, rep.Headline)
			for _, d := range rep.Details {
				fmt.Fprintln(a.Out, "  · "+d)
			}
			if len(rep.Next) > 0 {
				fmt.Fprintln(a.Out, "接下来：")
				for _, n := range rep.Next {
					fmt.Fprintln(a.Out, "  - "+n)
				}
			}
			return nil
		}}
}

// waitingGroup is the received sessions of one project that are not in an app
// yet, for one agent.
type waitingGroup struct {
	ref, agent string
	sessions   []string
}

// describeWaiting says, project by project, why conversations from the other
// computers are not in the apps here yet and what, if anything, to do about it.
func (a *App) describeWaiting(ctx context.Context, rep *checkReport, r *rt, received []listRow, blocked map[string]blockedEntry) {
	refOf := sessionRefs(r)
	groups := map[string]*waitingGroup{}
	var order []string
	loose := 0
	for _, x := range received {
		if _, held := blocked[x.SessionID]; held {
			continue // said above with its reason; it is not waiting for anything that will pass
		}
		ref := refOf[x.SessionID]
		if ref == "" {
			loose++
			continue
		}
		key := ref + "|" + x.Agent
		g := groups[key]
		if g == nil {
			g = &waitingGroup{ref: ref, agent: x.Agent}
			groups[key] = g
			order = append(order, key)
		}
		g.sessions = append(g.sessions, x.SessionID)
	}
	sort.Strings(order)
	mapped := map[string]session.ProjectMap{}
	for _, pm := range loadProjects(a.Cfg.StateDir()) {
		mapped[pm.ProjectRef] = pm
	}
	appWaits := map[string]*appWait{}
	for _, key := range order {
		g := groups[key]
		app := agentTitle(g.agent)
		ad, err := a.adapter(g.agent)
		if err != nil {
			rep.detail("来自别的电脑的 %d 个 %s 对话：这台电脑不支持装入 %s", len(g.sessions), app, app)
			continue
		}
		label := projectLabel(ctx, r, ad, g.ref, g.sessions, mapped)
		head := fmt.Sprintf("项目 %s（%d 个 %s 对话）", label, len(g.sessions), app)
		if pm, ok := mapped[g.ref]; ok {
			if st, err := os.Stat(pm.LocalPath); err != nil || !st.IsDir() {
				rep.raise("attention")
				rep.detail("%s：本机的项目文件夹 %s 不存在了，装不进去", head, pm.LocalPath)
				rep.next(fmt.Sprintf("把 %s 这个文件夹恢复出来（或 clone 回来），或运行 `%s project map %s --path <本机的项目文件夹>` 指向别的文件夹；之后下一轮同步会自动装入", pm.LocalPath, selfPath(), g.ref))
				continue
			}
			w := appWaits[g.agent]
			if w == nil {
				w = &appWait{}
				appWaits[g.agent] = w
			}
			w.projects = append(w.projects, label)
			w.sessions += len(g.sessions)
			continue
		}
		dir, note := autoProjectDirWith(ctx, a, r, g.ref, g.agent)
		if dir != "" {
			rep.detail("%s：本机还没有对应文件夹，下一轮同步会自动使用 %s 并把对话装进去", head, dir)
			continue
		}
		rep.raise("attention")
		rep.detail("%s：%s", head, explainNoFolder(note, label))
		rep.next(fmt.Sprintf("在本机放好（或 clone 回来）一个叫 %s 的文件夹，然后运行 `%s project map %s --path <那个文件夹>`；之后下一轮同步会自动装入", label, selfPath(), g.ref))
	}
	var agents []string
	for ag := range appWaits {
		agents = append(agents, ag)
	}
	sort.Strings(agents)
	for _, ag := range agents {
		w := appWaits[ag]
		app := agentTitle(ag)
		sort.Strings(w.projects)
		names := projectList(w.projects)
		head := fmt.Sprintf("%d 个来自别的电脑的 %s 对话（%d 个项目：%s）", w.sessions, app, len(w.projects), names)
		switch a.appState(ctx, ag) {
		case agent.NotRunning:
			rep.detail("%s：下一轮后台同步会装入", head)
		case agent.Running:
			rep.raise("attention")
			rep.detail("%s：%s 正在运行，不能往里写，完全退出后几分钟内自动装入", head, app)
			rep.next(fmt.Sprintf("请用户完全退出 %s（Cmd+Q）：几分钟内对话会自动装入，之后重新打开 %s 就能看到", app, app))
		default:
			rep.detail("%s：等 %s 完全退出后自动装入（现在判断不了它是否在运行）", head, app)
		}
	}
	if loose > 0 {
		rep.detail("另有 %d 个来自别的电脑的对话没有记录项目，只能在对方的电脑上继续", loose)
	}
}

// appWait is what waits for one app to be closed: projects whose folder is
// fine, so only the app holds them back.
type appWait struct {
	projects []string
	sessions int
}

// appState says whether the agent's app is running (Unknown when that cannot be told).
func (a *App) appState(ctx context.Context, agentName string) agent.RunState {
	ad, err := a.adapter(agentName)
	if err != nil {
		return agent.Unknown
	}
	home := a.Cfg.CodexHome()
	if agentName == "workbuddy" {
		home = a.Cfg.WorkBuddyHome()
	}
	st, _, err := ad.Runtime().Running(ctx, home, "")
	if err != nil {
		return agent.Unknown
	}
	return st
}

// explainNoFolder puts the reason autoProjectDirWith gave into words for the person.
func explainNoFolder(note, label string) string {
	switch {
	case strings.HasPrefix(note, "no folder named"):
		return fmt.Sprintf("本机找不到叫 %s 的文件夹，也不在对方 home 目录之下，没法自动决定放哪里", label)
	case strings.Contains(note, " folders named "):
		return "本机有不止一个同名文件夹，没法确定用哪一个：" + note
	case strings.HasPrefix(note, "the sessions do not record a folder"):
		return "这些对话没有记录项目文件夹，没法自动决定放哪里"
	}
	return "本机没法自动确定它的文件夹（" + note + "）"
}

// sessionRefs maps each session to its project.
func sessionRefs(r *rt) map[string]string {
	refOf := map[string]string{}
	if q, err := r.Store.DB().Query(`SELECT session_id, COALESCE(project_ref, '') FROM sessions`); err == nil {
		for q.Next() {
			var id, ref string
			if q.Scan(&id, &ref) == nil {
				refOf[id] = ref
			}
		}
		q.Close()
	}
	return refOf
}

// projectLabel is the name a person knows a project by: its label here, else
// the name of the folder its sessions were recorded in.
func projectLabel(ctx context.Context, r *rt, ad agent.Adapter, ref string, sessions []string, mapped map[string]session.ProjectMap) string {
	if pm, ok := mapped[ref]; ok {
		if pm.Label != "" {
			return pm.Label
		}
		return filepath.Base(pm.LocalPath)
	}
	if ad != nil {
		if origin := projectOrigin(ctx, r, ad, sessions); origin != "" {
			return filepath.Base(origin)
		}
	}
	return ref
}

// describeBlocked says which sessions the last relay round could not install,
// and why, one line per reason.
func (a *App) describeBlocked(ctx context.Context, rep *checkReport, r *rt, now time.Time, blocked map[string]blockedEntry) {
	refOf := sessionRefs(r)
	mapped := map[string]session.ProjectMap{}
	for _, pm := range loadProjects(a.Cfg.StateDir()) {
		mapped[pm.ProjectRef] = pm
	}
	type group struct {
		canAct   bool
		sessions int
		projects map[string][]string // ref -> sessions
		agent    string
		since    time.Time
	}
	groups := map[string]*group{}
	var order []string
	for id, e := range blocked {
		what, canAct := explainBlocked(e.Reason)
		g := groups[what]
		if g == nil {
			g = &group{canAct: canAct, projects: map[string][]string{}, agent: e.Agent}
			groups[what] = g
			order = append(order, what)
		}
		g.sessions++
		g.projects[refOf[id]] = append(g.projects[refOf[id]], id)
		if g.since.IsZero() || e.Since.Before(g.since) {
			g.since = e.Since
		}
	}
	sort.Strings(order)
	for _, what := range order {
		g := groups[what]
		ad, _ := a.adapter(g.agent)
		var names []string
		for ref, ids := range g.projects {
			if ref == "" {
				names = append(names, "未归入项目")
				continue
			}
			names = append(names, projectLabel(ctx, r, ad, ref, ids, mapped))
		}
		sort.Strings(names)
		rep.raise("attention")
		since := ""
		if !g.since.IsZero() {
			since = "，" + ago(now, g.since) + "起一直如此"
		}
		rep.detail("%d 个对话（%d 个项目：%s）%s：%s", g.sessions, len(names), projectList(names), since, what)
		if g.canAct {
			rep.next("把上面这条的原文告诉用户；不要自己改 WorkBuddy 或 Codex 的会话文件，也不要执行 adopt / choose，除非用户要求")
		}
	}
}
