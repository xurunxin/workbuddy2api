// checkin_report.go —— 手动签到入口（POST /v1/checkin）的进程内接线与报告组装。
//
// 为什么不 exec 外部 CLI 签到：号池的 credits 只由 scheduler.CheckinAll 写入
// （SetCreditsDetailed）。走 deploy/signin 签到虽然上游确实签到了，但**不会**
// 更新网关内存里的额度——这正是「签到成功、控制台积分却不刷新」的成因。故这里
// 要求的是一个能直接跑 CheckinAll 的回调。
package main

import (
	"errors"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
)

// checkinReportFn 把调度器的全量签到包装成 server.Config.CheckinFn 所需的签名。
//
// busy=true 表示已有一次签到在跑（scheduler.ErrBusy：手动入口与定时撞车），
// 由 handler 回 429 提示稍后再试，而不是当成失败。
func checkinReportFn(sch *scheduler.Scheduler, p *pool.Pool, cfg *Config) func() (server.CheckinReport, bool, error) {
	return func() (server.CheckinReport, bool, error) {
		outcomes, err := sch.CheckinAll()
		if errors.Is(err, scheduler.ErrBusy) {
			return server.CheckinReport{}, true, nil
		}
		if err != nil {
			return server.CheckinReport{}, false, err
		}
		// realm 由号池现查（CheckinOutcome 本身不带 realm，避免 scheduler 与 pool
		// 两处各存一份口径）；查不到留空，前端按「—」显示。
		realmOf := func(uid string) string {
			if a := p.AuthByUID(uid); a != nil {
				return a.Realm()
			}
			return ""
		}
		return buildCheckinReport(outcomes, realmOf,
			cfg.Schedule.CheckinEnabled, cfg.Schedule.CheckinHours), false, nil
	}
}

// buildCheckinReport 把 scheduler 的结果映射成 HTTP 响应体。
//
// 纯函数（realmOf 以闭包注入）是为了可单测：计数与 realm 归属错了不会报错，
// 只会让面板显示错，必须锁住。
func buildCheckinReport(outcomes []scheduler.CheckinOutcome, realmOf func(string) string,
	enabled bool, hours []int) server.CheckinReport {
	rep := server.CheckinReport{
		Enabled: enabled,
		Hours:   hours,
		Total:   len(outcomes),
		Results: make([]server.CheckinResult, 0, len(outcomes)),
	}
	for _, o := range outcomes {
		rep.Results = append(rep.Results, server.CheckinResult{
			UID:      o.UID,
			Nickname: o.Nickname,
			Realm:    realmOf(o.UID),
			Status:   string(o.Status),
			Credits:  o.Credits,
			Detail:   o.Detail,
		})
		switch o.Status {
		case scheduler.CheckinOK:
			rep.OK++
		case scheduler.CheckinAlready:
			rep.Already++
		case scheduler.CheckinFail:
			rep.Fail++
		case scheduler.CheckinSkipped:
			rep.Skipped++
		}
	}
	return rep
}
