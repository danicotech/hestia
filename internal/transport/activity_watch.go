package transport

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	activityv1 "github.com/danicotech/hestia/gen/hestia/activity/v1"
	"github.com/danicotech/hestia/gen/hestia/activity/v1/activityv1connect"
	"github.com/danicotech/hestia/internal/core/activity/betting"
	"github.com/danicotech/hestia/internal/core/activity/watch"
)

// 即時戰況推播的入口(watch.proto)。
//
// # 這個 handler 為什麼這麼短
//
// 它只做三件事:訂閱、把 Hub 給的東西轉成 proto 送出去、斷線時取消訂閱。
// **沒有任何資料庫查詢**,一個迴圈都沒有 —— 內容由 watchpg 的 LISTEN 迴圈
// 讀好之後餵進 Hub。這是刻意的:handler 一旦自己查資料庫,「同時有 200 個
// 觀眾」就會變成「每有一則變化就 200 次查詢」,而那正是當初不用輪詢的理由。
//
// # 匿名可讀
//
// 這裡沒有一行 requireUser,與 TournamentService 同一個理由(觀眾比選手多)。
// 附帶一提:認證攔截器是 connect.UnaryInterceptorFunc,本來就不會包住
// streaming handler —— 所以「匿名」在這條路徑上不是靠白名單,是結構使然。
// 也因此這支 RPC 絕不能回傳任何因人而異的內容(watch.proto 的「匿名可讀」),
// 而它確實沒有:送出去的位元組對同一屆的每個訂閱者完全相同。
//
// # 錯誤是「請重連」,不是「出事了」
//
// 串流結束的兩種非正常原因(跟不上被踢、伺服器關閉)對前端是同一個動作:
// 重連,然後重新拉一次完整狀態。所以它們回的 code 都是「可重試」那一類,
// 而且帶 reason 讓前端分得出來(Aborted 要立刻重連,Unavailable 要退避)。

// watchHeartbeatInterval 是心跳間隔。
//
// 20 秒是從部署形狀反推的:cloudflared 與多數反向代理的閒置逾時在 60~100 秒,
// 取三分之一以下才容得下一次丟包。太短則是純粹的浪費 —— 一屆賽事同時線上
// 200 人的話,每一秒的心跳就是每分鐘 12000 個空 frame。
const watchHeartbeatInterval = 20 * time.Second

// MountActivityWatch 把即時戰況服務掛上 mux。
//
// 為什麼不併進 MountActivity:那五個服務全部是請求/回應,依賴是領域服務;
// 這一個是長連線,依賴是一個行程內的扇出樞紐,而且它的生命週期(誰關閉 Hub)
// 由組裝端管。混在一起會讓 ActivityDeps 多背一個語意完全不同的欄位。
//
// hub 為 nil 時仍然會掛上去,但每次呼叫都回 Unimplemented —— 與 Deps 其餘
// 欄位同一個約定:半成品不會把整個進程帶走。
//
// opts 要傳與其他服務**同一組**攔截器。注意其中的 unary 攔截器
// (認證、event_logs)不會作用在 streaming 上,那是 connect 的設計,不是疏漏。
func MountActivityWatch(mux *http.ServeMux, hub *watch.Hub, opts ...connect.HandlerOption) {
	mux.Handle(activityv1connect.NewWatchServiceHandler(
		activityWatchHandler{hub: hub, heartbeat: watchHeartbeatInterval}, opts...))
}

// activityWatchHandler 是 WatchService 的實作。
//
// 直接吃 *watch.Hub 而不是在這裡宣告一個 port:Hub 已經是 core 的純型別
// (沒有 I/O、沒有 pgx),而任何替身都還是得產出 *watch.Subscription ——
// 那個介面能換掉的東西是空集合,只會多一層沒有人得益的間接。
type activityWatchHandler struct {
	hub       *watch.Hub
	heartbeat time.Duration
}

// WatchTournament 訂閱一屆賽事的全部變化,直到 client 斷線或伺服器關閉。
func (h activityWatchHandler) WatchTournament(
	ctx context.Context,
	req *connect.Request[activityv1.WatchTournamentRequest],
	stream *connect.ServerStream[activityv1.WatchTournamentResponse],
) error {
	if h.hub == nil {
		return unimplemented("WatchService")
	}
	slug := strings.TrimSpace(req.Msg.GetTournamentSlug())
	if slug == "" {
		return connectError(connect.CodeInvalidArgument, errWatchSlugRequired)
	}

	sub := h.hub.Subscribe(slug)
	// 這一行就是「client 斷線要能取消訂閱」的全部:不論是正常返回、送出失敗、
	// 還是 ctx 被取消,訂閱都會從 Hub 的索引裡消失。少了它,每一個關掉分頁的
	// 觀眾都會在 Hub 裡留下一個永遠不會被讀的 channel。
	defer sub.Close()

	// 第一則心跳 = 訂閱成立的信號。前端收到它才去拉完整狀態,
	// 「訂閱與拉取之間的缺口」因此在時序上不存在(watch.proto 檔頭的三步)。
	if err := stream.Send(watchHeartbeatMsg()); err != nil {
		return err
	}

	interval := h.heartbeat
	if interval <= 0 {
		interval = watchHeartbeatInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// 觀眾關掉分頁、網路斷掉、或伺服器正在關機。三者都不是錯誤:
			// 回 nil 讓 connect 正常收束這條串流。
			return nil

		case <-ticker.C:
			if err := stream.Send(watchHeartbeatMsg()); err != nil {
				// 送不出去通常就是對方已經走了。原樣回傳,不包成本套件的錯誤
				// —— 那是傳輸層的事實,不是領域錯誤。
				return err
			}

		case u, ok := <-sub.Updates():
			if !ok {
				return watchSubscriptionEnded(sub.Err())
			}
			msg := watchUpdateToProto(u)
			if msg == nil {
				// 收到一則本版本認不得的變化(滾動更新期間會發生)。
				// 跳過而不是中斷:一個不認得的種類不該讓觀眾的畫面斷線。
				continue
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}

var (
	// errWatchSlugRequired:沒指定要看哪一屆。
	errWatchSlugRequired = errors.New("必須指定要觀看的賽事 slug")
	// errWatchLagged:這條連線跟不上,被伺服器中斷。
	errWatchLagged = errors.New("推播落後過多,連線已中斷;請重連並重新取得完整狀態")
	// errWatchUnavailable:伺服器關閉中。
	errWatchUnavailable = errors.New("推播服務暫時無法使用,請稍後重連")
)

// watchSubscriptionEnded 把訂閱結束的原因翻成 client 看得懂的指示。
//
// 兩個都是「重連」,但退避方式不同,所以不能收斂成同一個 code:
//
//	Aborted      跟不上 —— 立刻重連是對的(問題出在這條連線,不在伺服器)
//	Unavailable  伺服器關閉中 —— 要退避,馬上重連只會再被拒絕一次
//
// reason 直接掛在錯誤上而不是進 errorCodes:那張表服務的是 unary 的
// 領域錯誤,而這兩個是串流專屬的傳輸狀態,沒有第二個呼叫端會用到。
func watchSubscriptionEnded(err error) error {
	switch {
	case errors.Is(err, watch.ErrLagged):
		return watchStreamError(connect.CodeAborted, errWatchLagged, "watch_lagged")
	case errors.Is(err, watch.ErrHubClosed):
		return watchStreamError(connect.CodeUnavailable, errWatchUnavailable, "watch_unavailable")
	default:
		// 訂閱被自己這一側關掉(正常結束)。
		return nil
	}
}

// watchStreamError 產生帶 reason 的可信錯誤。
func watchStreamError(code connect.Code, err error, reason string) error {
	out := connectError(code, err)
	var ce *connect.Error
	if errors.As(out, &ce) {
		ce.Meta().Set(ErrorReasonHeader, reason)
	}
	return out
}

// watchHeartbeatMsg 是一則心跳。理由見 watch.proto 的 Heartbeat。
func watchHeartbeatMsg() *activityv1.WatchTournamentResponse {
	return &activityv1.WatchTournamentResponse{
		EmittedAt: tsPB(time.Now().UTC()),
		Update:    &activityv1.WatchTournamentResponse_Heartbeat{Heartbeat: &activityv1.Heartbeat{}},
	}
}

// watchUpdateToProto 把一則變化轉成串流訊息;回 nil = 認不得的種類,跳過。
//
// 轉換一律沿用既有的共用函式(matchToProto / matchHandicapsToProto /
// phaseToProto / rankToProto)—— 對戰表頁查詢回的與推播送的必須是同一個東西,
// 在這裡另寫一份,遲早會有一邊漏掉某個欄位,而症狀只是「即時更新之後
// 某個欄位變空了」,沒有人會當成 bug。
func watchUpdateToProto(u watch.Update) *activityv1.WatchTournamentResponse {
	out := &activityv1.WatchTournamentResponse{EmittedAt: tsPB(u.EmittedAt)}
	switch u.Kind {
	case watch.KindMatch:
		if u.Match == nil {
			return nil
		}
		out.Update = &activityv1.WatchTournamentResponse_Match{
			Match: &activityv1.MatchUpdate{Match: matchToProto(*u.Match)},
		}
	case watch.KindHandicapLocked:
		if u.Handicaps == nil {
			return nil
		}
		out.Update = &activityv1.WatchTournamentResponse_HandicapLocked{
			HandicapLocked: &activityv1.HandicapLockedUpdate{
				Handicaps: matchHandicapsToProto(u.Handicaps),
			},
		}
	case watch.KindOdds:
		if u.Odds == nil {
			return nil
		}
		out.Update = &activityv1.WatchTournamentResponse_Odds{
			Odds: &activityv1.OddsUpdate{Odds: watchOddsToProto(u.Odds)},
		}
	case watch.KindPhase:
		out.Update = &activityv1.WatchTournamentResponse_Phase{
			Phase: &activityv1.PhaseUpdate{Phase: phaseToProto(u.Phase)},
		}
	case watch.KindChampion:
		if u.Champion == nil {
			return nil
		}
		c := u.Champion
		out.Update = &activityv1.WatchTournamentResponse_Champion{
			Champion: &activityv1.ChampionUpdate{
				FinalMatchPublicId:     c.FinalMatchPublicID,
				ChampionPlayerPublicId: c.PlayerPublicID,
				ChampionDisplayName:    c.DisplayName,
				ChampionRank:           rankToProto(c.Rank),
				RunnerUpPlayerPublicId: c.RunnerUpPublicID,
				RunnerUpDisplayName:    c.RunnerUpDisplayName,
			},
		}
	default:
		return nil
	}
	return out
}

// watchOddsToProto 把賠率檢視轉成 proto。
//
// 名字帶 watch 前綴是暫時的:BettingService 的 GetOdds 需要一模一樣的轉換,
// 那支 handler 落地時這兩個應該合成一個(整合時處理,見回報)。
// 差別只有一個 —— 這裡的 my_vote 恆為 0,因為推播是廣播,不可能因人而異。
func watchOddsToProto(o *betting.MatchOdds) *activityv1.MatchOdds {
	return &activityv1.MatchOdds{
		MatchPublicId: o.MatchPublicID,
		P1Votes:       int32(o.P1Votes),
		P2Votes:       int32(o.P2Votes),
		P1OddsMilli:   o.P1OddsMilli,
		P2OddsMilli:   o.P2OddsMilli,
		OpenForBets:   o.OpenForBets,
		MyVote:        0,
	}
}
