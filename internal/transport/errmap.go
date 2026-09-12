package transport

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/danicotech/hestia/internal/core/activity/activityerr"
	"github.com/danicotech/hestia/internal/core/activity/betting"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/prize"
	"github.com/danicotech/hestia/internal/core/activity/signup"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/core/platform/activitylog"
	"github.com/danicotech/hestia/internal/core/platform/adminecon"
	"github.com/danicotech/hestia/internal/core/platform/daily"
	"github.com/danicotech/hestia/internal/core/platform/identity"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/core/platform/notification"
	"github.com/danicotech/hestia/internal/core/platform/play"
	"github.com/danicotech/hestia/internal/core/platform/shop"
)

// ErrorReasonHeader 是機器可讀的錯誤成因,隨錯誤一起送出去。
//
// ## 為什麼非有不可
//
// code 太粗。`FailedPrecondition` 同時代表「這個 Discord 帳號還沒綁定」、
// 「餘額不足」、「超過限購」、「退款窗口關了」—— 四件事的處置完全不同
// (引導登入 / 顯示餘額 / 顯示上限 / 告知已過期),而閘道只拿得到 code。
// 唯一的替代方案是比對中文訊息,那等於把**人類可讀的文案**變成 API 契約:
// 我們改一次措辭,呼叫端就靜靜地退化成通用訊息。
//
// ## 為什麼是 metadata 而不是 error details(protobuf Any)
//
//  1. **三種協定都過得去。** Connect 走 HTTP header、gRPC 與 gRPC-Web 走
//     trailer,connect-go 兩邊都會併進 `connect.Error.Meta()`。
//     details 只在 Connect JSON 與 gRPC 有,而且呼叫端要先取得型別才讀得懂。
//  2. **不必為錯誤分類再發明一份 proto 訊息。** reason 是一個穩定的識別字,
//     用 Any 包一層只是多一個要跟著版本走的型別。
//  3. **讀起來便宜。** 呼叫端一行 `err.meta.get(...)`,不必解 Any。
//
// 值域是封閉的:全部列在 errorCodes,並在 proto/README.md 對外公告。
const ErrorReasonHeader = "Hestia-Error-Reason"

// errorCodes 是 sentinel error → (connect code, reason) 的**唯一**映射表。
//
// 集中一處的理由:同一個領域錯誤在不同 handler 回不同 code,對 client 是災難
// (重試策略全錯)。要加新錯誤就加在這裡,不要在 handler 裡臨時判斷。
// reason 與 code 放同一列也是這個理由的延伸 —— 兩者分開放,遲早會有錯誤
// 只更新其中一半。
//
// reason 命名原則:
//   - snake_case,**穩定**。它是契約的一部分,改名等同破壞性變更;
//     訊息文案可以隨時改,reason 不行。
//   - 描述**成因**,不是 code 的同義詞(insufficient_balance,
//     不是 failed_precondition)。
//   - 同名不同義的加來源前綴(daily / shop 各有一個 ErrLedgerStateConflict,
//     兩者的處置不同,所以是兩個 reason)。
//
// code 選擇原則:
//   - InvalidArgument:請求本身不合法,重送同樣的內容一定還是失敗。
//   - FailedPrecondition:請求合法,但系統當前狀態不允許(餘額、窗口、狀態機)。
//     刻意不用 ResourceExhausted——它在 connect 對應 HTTP 429,client 會當成
//     限流而重試,但「餘額不足 / 已達限購」重試永遠不會成功。
//   - AlreadyExists:這件事已經發生過了(今天簽過、這筆已退過)。
//   - Aborted:併發衝突,client 應該重新讀狀態後再決定。
//   - PermissionDenied:身分有效但這東西不是你的。
//   - Internal:兩個權威互相矛盾等異常狀態——必須失敗出聲,不能靜靜回成功。
var errorCodes = []struct {
	err    error
	code   connect.Code
	reason string
}{
	// ── 遊戲 / 開箱 / 抽獎 / 寵物(play,schemas/25)────────────────

	// ── 活動層:報名與身分(signup)────────────────────────────
	//
	// ErrInvalidCredentials 刻意不區分「查無此遊戲ID」與「通行碼錯誤」——
	// 遊戲ID 是公開資訊(對戰表上就有),區分等於送出一份帳號列舉工具。
	{signup.ErrInvalidCredentials, connect.CodeUnauthenticated, "activity_invalid_credentials"},
	{signup.ErrAlreadyRegistered, connect.CodeAlreadyExists, "activity_already_registered"},
	{signup.ErrGameIDRequired, connect.CodeInvalidArgument, "activity_game_id_required"},
	{signup.ErrInvalidGameID, connect.CodeInvalidArgument, "activity_invalid_game_id"},
	{signup.ErrDiscordNameRequired, connect.CodeInvalidArgument, "activity_discord_name_required"},
	{signup.ErrFieldTooLong, connect.CodeInvalidArgument, "activity_field_too_long"},
	// 兩種「已綁」分開:前者要使用者先解綁自己的,後者是那個帳號被別人用了。
	{signup.ErrAlreadyBound, connect.CodeFailedPrecondition, "activity_player_already_bound"},
	{signup.ErrUserAlreadyBound, connect.CodeFailedPrecondition, "activity_user_already_bound"},
	{signup.ErrUserRequired, connect.CodeUnauthenticated, "activity_platform_account_required"},

	// ── 活動層:賽事與階段(tournament)────────────────────────
	{tournament.ErrInvalidRequest, connect.CodeInvalidArgument, "tournament_invalid_request"},
	// 設定壞掉在**建立賽事**這條路上是致命的(NewConfig),所以有 code 可回;
	// 讀既存 config 時壞欄位會退回預設而不產生錯誤,走不到這裡。
	{tournament.ErrConfigMalformed, connect.CodeInvalidArgument, "tournament_invalid_config"},
	// 代號格式錯與代號被占用分兩個 reason:前者要改寫法,後者換一個就好。
	// 合成一個的話,裁判只會看到「代號不行」然後開始亂猜。
	{tournament.ErrInvalidSlug, connect.CodeInvalidArgument, "tournament_invalid_slug"},
	{tournament.ErrSlugTaken, connect.CodeAlreadyExists, "tournament_slug_taken"},
	{tournament.ErrCommunityNotFound, connect.CodeNotFound, "community_not_found"},
	{tournament.ErrTournamentNotFound, connect.CodeNotFound, "tournament_not_found"},
	{tournament.ErrPlayerNotFound, connect.CodeNotFound, "activity_player_not_found"},
	// Aborted 而不是 FailedPrecondition:階段被別人搶先改了,重讀後重試是合理的。
	{tournament.ErrPhaseConflict, connect.CodeAborted, "tournament_phase_conflict"},
	{tournament.ErrWrongPhase, connect.CodeFailedPrecondition, "tournament_wrong_phase"},
	{tournament.ErrIllegalTransition, connect.CodeFailedPrecondition, "tournament_illegal_transition"},
	{tournament.ErrRanksLocked, connect.CodeFailedPrecondition, "tournament_ranks_locked"},
	{tournament.ErrPlayersUnranked, connect.CodeFailedPrecondition, "tournament_players_unranked"},
	{tournament.ErrInvalidRank, connect.CodeInvalidArgument, "activity_invalid_rank"},
	{tournament.ErrActorRequired, connect.CodeInvalidArgument, "activity_actor_required"},
	{tournament.ErrInvalidSeed, connect.CodeInvalidArgument, "tournament_invalid_seed"},
	{tournament.ErrNoDrawablePlayers, connect.CodeFailedPrecondition, "tournament_no_drawable_players"},
	{tournament.ErrPlayerNotSeeded, connect.CodeFailedPrecondition, "tournament_player_not_seeded"},
	{tournament.ErrSamePlayer, connect.CodeInvalidArgument, "tournament_same_player"},

	// ── 活動層:讓武 BP(handicap)──────────────────────────────
	//
	// BP 不足是 FailedPrecondition 不是 ResourceExhausted:BP 不是配額,
	// 是這一場的固定預算,退掉一項就能再買 —— 前端該顯示餘額而不是「稍後再試」。
	{handicap.ErrInvalidRequest, connect.CodeInvalidArgument, "handicap_invalid_request"},
	{handicap.ErrInsufficientBP, connect.CodeFailedPrecondition, "insufficient_bp"},
	{handicap.ErrNoBudget, connect.CodeFailedPrecondition, "handicap_no_budget"},
	{handicap.ErrHandicapLocked, connect.CodeFailedPrecondition, "handicap_locked"},
	{handicap.ErrAlreadyLocked, connect.CodeFailedPrecondition, "handicap_already_locked"},
	{handicap.ErrHandicapClosed, connect.CodeFailedPrecondition, "handicap_wrong_phase"},
	// 階段對但裁判還沒按開盤 —— 對選手的下一步不同(等公告 vs 這場不選購),故分開。
	{handicap.ErrHandicapNotOpen, connect.CodeFailedPrecondition, "handicap_not_open"},
	{handicap.ErrTargetNoteRequired, connect.CodeInvalidArgument, "handicap_target_note_required"},
	{handicap.ErrNotSelectionOwner, connect.CodePermissionDenied, "handicap_not_selection_owner"},
	{handicap.ErrItemNotFound, connect.CodeNotFound, "handicap_item_not_found"},
	{handicap.ErrSelectionNotFound, connect.CodeNotFound, "handicap_selection_not_found"},
	{handicap.ErrSelectionAlreadyVoided, connect.CodeAlreadyExists, "handicap_selection_already_voided"},
	// spent 與選購紀錄對不起來:這是寫入路徑有 bug,不是使用者做錯什麼。
	{handicap.ErrBudgetInconsistent, connect.CodeInternal, "handicap_budget_inconsistent"},

	// ── 活動層:投票與下注(betting)──────────────────────────
	{betting.ErrInvalidRequest, connect.CodeInvalidArgument, "bet_invalid_request"},
	{betting.ErrSelfBet, connect.CodePermissionDenied, "bet_self_bet"},
	{betting.ErrBettingClosed, connect.CodeFailedPrecondition, "bet_closed"},
	{betting.ErrVotingClosed, connect.CodeFailedPrecondition, "vote_closed"},
	// 賠率在送達前跑掉了。Aborted:重讀賠率後重試是正確的處置。
	{betting.ErrOddsMoved, connect.CodeAborted, "bet_odds_moved"},
	{betting.ErrStakeTooLarge, connect.CodeFailedPrecondition, "bet_stake_too_large"},
	{betting.ErrDuplicateLeg, connect.CodeInvalidArgument, "bet_duplicate_leg"},
	{betting.ErrTooManyLegs, connect.CodeInvalidArgument, "bet_too_many_legs"},
	{betting.ErrMatchNotInTournament, connect.CodeInvalidArgument, "bet_match_not_in_tournament"},
	{betting.ErrMatchNotDecided, connect.CodeFailedPrecondition, "bet_match_not_decided"},
	{betting.ErrWalkoverMatch, connect.CodeFailedPrecondition, "bet_walkover_match"},
	{betting.ErrNotWalkover, connect.CodeFailedPrecondition, "bet_not_walkover"},
	{betting.ErrLedgerStateConflict, connect.CodeInternal, "bet_ledger_state_conflict"},

	// ── 活動層:比賽生命週期(match)──────────────────────────
	{match.ErrInvalidRequest, connect.CodeInvalidArgument, "match_invalid_request"},
	{activityerr.ErrMatchNotFound, connect.CodeNotFound, "activity_match_not_found"},
	{match.ErrConfirmationRequired, connect.CodeInvalidArgument, "confirmation_required"},
	{match.ErrWinnerNotInMatch, connect.CodeInvalidArgument, "winner_not_in_match"},
	{match.ErrNotReady, connect.CodeFailedPrecondition, "match_not_ready"},
	{match.ErrNotLocked, connect.CodeFailedPrecondition, "match_not_locked"},
	{match.ErrNotLive, connect.CodeFailedPrecondition, "match_not_live"},
	{match.ErrMatchFinished, connect.CodeFailedPrecondition, "match_finished"},
	{match.ErrAlreadyOpen, connect.CodeFailedPrecondition, "match_already_open"},
	{match.ErrAlreadyWithdrawn, connect.CodeAlreadyExists, "activity_player_already_withdrawn"},
	{match.ErrPlayerWithdrawn, connect.CodeFailedPrecondition, "activity_player_withdrawn"},
	{activityerr.ErrPlayersNotSet, connect.CodeFailedPrecondition, "match_players_not_set"},
	// 對戰表或晉級目標不見了:資料完整性失效,不是使用者錯誤。
	{match.ErrBracketMissing, connect.CodeInternal, "activity_bracket_missing"},
	{match.ErrAdvanceTargetMissing, connect.CodeInternal, "activity_advance_target_missing"},

	{play.ErrNotFound, connect.CodeNotFound, "play_not_found"},
	{play.ErrDisabled, connect.CodeFailedPrecondition, "play_disabled"},
	// 到上限不是錯誤是規則。用 ResourceExhausted 讓 client 分得出
	// 「今天不能再玩」與「這次請求壞了」。
	{play.ErrDailyLimit, connect.CodeResourceExhausted, "play_daily_limit"},
	{play.ErrPoolEmpty, connect.CodeFailedPrecondition, "play_pool_empty"},
	{play.ErrAlreadyEntered, connect.CodeAlreadyExists, "play_already_entered"},
	{play.ErrClosed, connect.CodeFailedPrecondition, "play_closed"},
	{play.ErrNotOwner, connect.CodePermissionDenied, "play_not_owner"},
	{play.ErrInvalidParams, connect.CodeInvalidArgument, "play_invalid_params"},

	// ── 活動層:賽事獎金(prize)────────────────────────────────
	{prize.ErrInvalidRequest, connect.CodeInvalidArgument, "prize_invalid_request"},
	// 季軍推不出來是賽制的事實,不是暫時性失敗:單淘汰沒有季軍賽,重試永遠一樣。
	{prize.ErrThirdPlaceUndecidable, connect.CodeFailedPrecondition, "prize_third_place_undecidable"},
	{prize.ErrFinalNotDecided, connect.CodeFailedPrecondition, "prize_final_not_decided"},
	// 以下兩個是資料異常,不是裁判能修正的東西 —— 給 internal,讓它進日誌而不是
	// 變成一句叫裁判自己想辦法的提示。
	{prize.ErrBracketBroken, connect.CodeInternal, "prize_bracket_broken"},
	{prize.ErrLedgerStateConflict, connect.CodeInternal, "prize_ledger_state_conflict"},

	// ── 帳本(ledger)──────────────────────────────────────────────
	{ledger.ErrInsufficientBalance, connect.CodeFailedPrecondition, "insufficient_balance"},
	{ledger.ErrIdempotencyConflict, connect.CodeAborted, "idempotency_conflict"},
	// 同一把冪等鍵正在處理中:稍後重試會成功(拿到原本的結果),
	// 用 Unavailable 讓 client 的標準重試策略自然接手。
	{ledger.ErrInFlight, connect.CodeUnavailable, "idempotency_in_flight"},
	{ledger.ErrInvalidOp, connect.CodeInvalidArgument, "invalid_ledger_op"},

	// ── 簽到(daily)──────────────────────────────────────────────
	{daily.ErrAlreadyClaimed, connect.CodeAlreadyExists, "daily_already_claimed"},
	{daily.ErrTimezoneChangeCooldown, connect.CodeFailedPrecondition, "timezone_change_cooldown"},
	{daily.ErrLedgerStateConflict, connect.CodeInternal, "daily_ledger_state_conflict"},
	{daily.ErrInvalidConfig, connect.CodeInternal, "daily_invalid_config"},

	// ── 商店(shop)──────────────────────────────────────────────
	{shop.ErrInvalidRequest, connect.CodeInvalidArgument, "shop_invalid_request"},
	{shop.ErrUserNotFound, connect.CodeNotFound, "user_not_found"},
	{shop.ErrItemNotFound, connect.CodeNotFound, "item_not_found"},
	{shop.ErrEntitlementNotFound, connect.CodeNotFound, "entitlement_not_found"},
	{shop.ErrRedemptionNotFound, connect.CodeNotFound, "redemption_not_found"},
	{shop.ErrItemNotListed, connect.CodeFailedPrecondition, "item_not_listed"},
	{shop.ErrPerUserLimitReached, connect.CodeFailedPrecondition, "per_user_limit_reached"},
	{shop.ErrAlreadyRevoked, connect.CodeFailedPrecondition, "entitlement_already_revoked"},
	{shop.ErrRefundWindowClosed, connect.CodeFailedPrecondition, "refund_window_closed"},
	{shop.ErrRedemptionNotPending, connect.CodeFailedPrecondition, "redemption_not_pending"},
	{shop.ErrNotEntitlementOwner, connect.CodePermissionDenied, "not_entitlement_owner"},
	{shop.ErrNotRedemptionOwner, connect.CodePermissionDenied, "not_redemption_owner"},
	{shop.ErrLedgerStateConflict, connect.CodeInternal, "shop_ledger_state_conflict"},

	// ── 管理員經濟操作(adminecon)──────────────────────────────
	{adminecon.ErrReasonRequired, connect.CodeInvalidArgument, "reason_required"},
	{adminecon.ErrInvalidInput, connect.CodeInvalidArgument, "invalid_input"},
	{adminecon.ErrEntryNotFound, connect.CodeNotFound, "entry_not_found"},
	{adminecon.ErrNotRefundable, connect.CodeFailedPrecondition, "entry_not_refundable"},
	{adminecon.ErrAlreadyRefunded, connect.CodeAlreadyExists, "entry_already_refunded"},

	// ── 活動記錄(activitylog)──────────────────────────────────
	// ErrActorNotLinked 用 FailedPrecondition 而不是 NotFound / Unauthenticated:
	// 請求本身沒問題,是「這個 Discord 使用者還沒綁定平台帳號」這個系統狀態
	// 擋住了。呼叫端(stentor)據此知道該引導綁定,而不是重試 ——
	// 重試一萬次也不會有人自己出現在 identities 裡。
	{activitylog.ErrActorNotLinked, connect.CodeFailedPrecondition, "actor_not_linked"},
	{activitylog.ErrSpaceNotRegistered, connect.CodeFailedPrecondition, "space_not_registered"},
	{activitylog.ErrInvalidRequest, connect.CodeInvalidArgument, "activity_invalid_request"},

	// ── 身分 / session(identity)────────────────────────────────
	// 憑證類錯誤一律 Unauthenticated:對 client 的意思都是「重新登入」。
	{identity.ErrInvalidToken, connect.CodeUnauthenticated, "invalid_token"},
	{identity.ErrTokenExpired, connect.CodeUnauthenticated, "token_expired"},
	{identity.ErrSessionNotFound, connect.CodeUnauthenticated, "session_not_found"},
	{identity.ErrSessionRevoked, connect.CodeUnauthenticated, "session_revoked"},
	{identity.ErrSessionExpired, connect.CodeUnauthenticated, "session_expired"},
	{identity.ErrTokenReuseDetected, connect.CodeUnauthenticated, "token_reuse_detected"},
	// state 相關:是「這次回呼的參數有問題」,不是「你沒登入」。
	{identity.ErrInvalidState, connect.CodeInvalidArgument, "invalid_oauth_state"},
	{identity.ErrStateExpired, connect.CodeInvalidArgument, "oauth_state_expired"},
	// ErrStateMismatch 刻意用 PermissionDenied 而不是 Unauthenticated:
	// 它的意思是「這次回呼不屬於這個瀏覽器」——那是 login CSRF 的訊號,
	// 不是「請你登入」。用 denied 也讓它在 event_logs 裡一眼可見。
	{identity.ErrStateMismatch, connect.CodePermissionDenied, "oauth_state_mismatch"},
	{identity.ErrInvalidRedirect, connect.CodeInvalidArgument, "invalid_redirect_uri"},
	// 外部 provider 掛了/超時:稍後重試有意義。
	{identity.ErrProviderExchange, connect.CodeUnavailable, "provider_exchange_failed"},
	{identity.ErrAccountDeleted, connect.CodePermissionDenied, "account_deleted"},
	{identity.ErrInvalidConfig, connect.CodeInternal, "identity_invalid_config"},

	// ── 通知拉取(notification)──────────────────────────────────
	{notification.ErrInvalidRequest, connect.CodeInvalidArgument, "notification_invalid_request"},

	// ── 入口層自己的憑證錯誤(interceptors.go)────────────────────
	// 它們不是 core sentinel,但**同樣需要穩定的成因**:呼叫端要分得出
	// 「我兩種憑證都帶了」與「這支 RPC 不接受代打」,前者是自己的 bug,
	// 後者是配置問題,兩者都不該靠比對中文訊息來判斷。
	{errMixedCredentials, connect.CodePermissionDenied, "mixed_credentials"},
	{errServiceOnUserRPC, connect.CodePermissionDenied, "service_on_user_rpc"},
	{errDelegationNotGranted, connect.CodePermissionDenied, "delegation_not_granted"},
	{errUserOnServiceRPC, connect.CodePermissionDenied, "user_on_service_rpc"},
	{errActingUserRequired, connect.CodeInvalidArgument, "acting_user_required"},
	// X-Acting-User 的兩種錯誤各有 reason。**這是「不要用訊息帶值」的替代品**:
	// 呼叫端想知道「是格式錯還是 provider 不支援」,答案在 reason,
	// 不在一段引用了它自己輸入的中文訊息裡(那條路會把任意字串寫進稽核表)。
	{errActingUserFormat, connect.CodeInvalidArgument, "acting_user_format"},
	{errActingUserProvider, connect.CodeInvalidArgument, "acting_user_provider_unsupported"},

	// ── 瀏覽器登入流程(browserauth.go)────────────────────────────
	// 這三個**必須**在這張表上,不能自成一套字串:回呼失敗時錯誤碼會進
	// 導回網址(/?login_error=<reason>),前端要用它挑文案。前端不該為了
	// 「從 RPC 回來的」與「從網址回來的」維護兩份錯誤字典 —— 那正是
	// 專案第 9 條要避免的重複。
	//
	// 三者都刻意**不帶任何外部輸入**:回呼的 query 全都是攻擊者可自由控制的
	// 字串(那個 URL 誰都能組),把 provider 回的 error 原樣塞進導回網址
	// 等於開一條反射式注入通道。值域封閉,對不上的一律折成 login_failed。
	{errOAuthDenied, connect.CodePermissionDenied, "oauth_denied"},
	{errOAuthCallbackInvalid, connect.CodeInvalidArgument, "oauth_callback_invalid"},
	{errLoginFailed, connect.CodeInternal, "login_failed"},

	// ── 入口層自己的 port 錯誤 ──────────────────────────────────
	{ErrNotFound, connect.CodeNotFound, "not_found"},
	{ErrUnauthenticated, connect.CodeUnauthenticated, "unauthenticated"},
	{ErrPermissionDenied, connect.CodePermissionDenied, "permission_denied"},

	// ── context ────────────────────────────────────────────────────
	{context.Canceled, connect.CodeCanceled, "canceled"},
	{context.DeadlineExceeded, connect.CodeDeadlineExceeded, "deadline_exceeded"},
}

var (
	// errInternal 是未知錯誤對外的固定訊息。
	// 未映射的錯誤可能包含 SQL、連線字串、內部 id——一律不外洩,原文只進 log。
	errInternal = errors.New("內部錯誤")
	// errUntrusted 給「別人做的 connect.Error」用:code 保留(client 的重試
	// 策略需要),訊息換掉。
	errUntrusted = errors.New("請求無法完成")
)

// ownError 標記「這個 connect.Error 是本套件產生的,訊息可信」。
//
// 為什麼需要標記:port 的實作不在本套件控制之下,它若回一個
// *connect.Error,原本會被原樣放行,訊息直接進回應與 event_logs——
// 那是一條沒有防線的洩漏通道(SQL、內部 id、他人資料都可能在裡面)。
// 有了標記,只有我們自己寫的文案能出去。
type ownError struct{ err *connect.Error }

func (e ownError) Error() string { return e.err.Error() }
func (e ownError) Unwrap() error { return e.err }

// reasonFor 查出這個錯誤的機器可讀成因;查不到回空字串。
//
// 查不到就**不帶** reason,這是刻意的(而且是安全要求):未映射的錯誤
// 對外一律是 Internal + 固定文案,替它們編一個 reason 等於把內部錯誤的
// 分類洩漏出去 —— 「你送的東西讓資料庫的哪一段炸了」不是契約的一部分。
//
// 同理,invalidArgument() 與 unimplemented() 這類臨時產生的錯誤也沒有 reason:
// 它們的 code 已經說完了全部(參數不合法 / 功能未上線),沒有第二種成因要分辨。
func reasonFor(err error) string {
	for _, m := range errorCodes {
		if errors.Is(err, m.err) {
			return m.reason
		}
	}
	return ""
}

// connectError 產生「可信」的 connect 錯誤。本套件一律用它,不要直接
// connect.NewError——直接建的沒有標記,訊息會在稽核時被當成外部來源丟掉。
//
// reason 在這裡附上(而不是只在 toConnectError):攔截器是直接呼叫這個函式的,
// 少了這一步,憑證類錯誤就會是全服務唯一一批沒有成因標記的錯誤。
// metadata 對 Connect / gRPC / gRPC-Web 三種協定都送得出去。
func connectError(code connect.Code, err error) error {
	ce := connect.NewError(code, err)
	if reason := reasonFor(err); reason != "" {
		ce.Meta().Set(ErrorReasonHeader, reason)
	}
	return ownError{ce}
}

// ErrorReason 取出錯誤上的成因標記(給測試與呼叫端範例用);沒有就是空字串。
func ErrorReason(err error) string {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return ""
	}
	return ce.Meta().Get(ErrorReasonHeader)
}

// trusted 回報錯誤是否由本套件產生。
func trusted(err error) bool {
	var own ownError
	return errors.As(err, &own)
}

// toConnectError 把任何錯誤轉成帶正確 code 的連線層錯誤。
//
//	本套件產生的          → 原樣放行(訊息是寫給使用者看的)
//	認得的 core sentinel  → 表上的 code + sentinel 自己的訊息
//	別人做的 connect.Error → 保留 code,訊息換成安全文案
//	其他                  → Internal + 固定訊息,原文只進 log
func toConnectError(err error) error {
	if err == nil {
		return nil
	}
	if trusted(err) {
		return err
	}
	for _, m := range errorCodes {
		if errors.Is(err, m.err) {
			return connectError(m.code, m.err)
		}
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		return connectError(ce.Code(), errUntrusted)
	}
	return connectError(connect.CodeInternal, errInternal)
}

// invalidArgument 是 handler 做參數檢查時的統一產生器
// (proto 沒有 required,必填欄位只能在這裡擋)。
func invalidArgument(msg string) error {
	return connectError(connect.CodeInvalidArgument, errors.New(msg))
}

// unimplemented 給尚未注入實作的服務用。回 Unimplemented 而不是 Internal,
// client 才知道「這個功能還沒上線」而不是「伺服器壞了」。
func unimplemented(name string) error {
	return connectError(connect.CodeUnimplemented, errors.New(name+" 尚未提供"))
}
