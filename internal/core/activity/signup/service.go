package signup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
)

// Service 是報名與選手身分的唯一入口。
//
// # 明碼通行碼的流向
//
// 明碼只存在於**一個**回傳值裡:RegeneratePasscode(給裁判)。
// Register 也會產生一組(passcode_hash 是 NOT NULL),但**不回傳** ——
// 登入用不到它,吐一組沒有用途的密碼只會讓人以為那是要保存的東西。
//
// 它不進參數、不進 Repo、不進錯誤訊息、**不進任何日誌** ——
// 這個套件裡沒有任何一行會把通行碼寫到 return 以外的地方,
// 而這是刻意維持的性質,不是碰巧。加日誌前請先想清楚這一句。
type Service struct {
	repo   Repo
	hasher *Hasher
}

// NewService 建立報名服務。
func NewService(repo Repo, hasher *Hasher) *Service {
	return &Service{repo: repo, hasher: hasher}
}

// RegisterParams 是一份報名表。
//
// **只有 GameID 是必填的**(2026-09-13 定案,推翻了 Discord 名稱必填)。
// 其餘每一欄留空都能報名成功 —— 報名的門檻要低到「知道自己遊戲ID 就能報」,
// 每多一個必填欄位就在報名頁上攔掉一部分人。
//
// 選填的那些是**給裁判評段用的參考資料**:評段要綜合論劍段位、積分、實戰經驗與
// 整體 PVP 實力,所以表單問得比「你叫什麼」多;問得多不等於要求得多,
// 沒填就是裁判少一份參考。
type RegisterParams struct {
	TournamentSlug string
	// GameID 全服唯一,**唯一必填欄位**。報過往屆的話會自動接上既有的選手檔案。
	GameID string
	// DisplayName 留空則沿用 GameID。
	DisplayName string
	// DiscordName 選填。裁判聯絡選手(排輪次、通知封盤)用,沒填就只能在遊戲裡找人 ——
	// 那是報名者自己的取捨,不是把他擋在門外的理由。
	DiscordName string
	// SelfRatedRank 自評段位;RankUnspecified = 沒填。
	// 除了當評段起點,也讓裁判看得出誰高估或低估自己。
	SelfRatedRank    bp.Rank
	LadderRank       string
	LadderScore      int32
	ArtsNote         string
	AvailabilityNote string
}

// RegisterResult 是報名的結果。
//
// **刻意沒有通行碼欄位**(2026-09-13):Register 仍然會產生並存一組雜湊
// (passcode_hash 是 NOT NULL,而 passcode_issued_at 是 session 作廢的依據),
// 但登入只要遊戲ID,明碼對選手沒有任何用途。結構裡沒有這個欄位,
// 就不可能有人在 transport 那端「順手」把它放進回應。
type RegisterResult struct {
	Player tournament.Player
	Fencer Fencer
	// ReturningFencer = 這個遊戲ID 在往屆報過名,已接上既有檔案。
	ReturningFencer bool
	// PreviousRank 是往屆最後一次的評定段位;RankUnspecified = 初次參賽。
	// 這**不是**本屆段位 —— 本屆要等裁判重新評定。
	PreviousRank bp.Rank
}

// Register 報名一屆賽事。
//
// 只在 signup 階段開放,而且**檢查做兩次**:這裡讀一次,寫入時再帶著
// RequirePhase 讓 SQL 的 WHERE 再擋一次。兩次之間裁判剛好封閉報名的機率不高,
// 但報名截止後還混進一個人,對已經抽好的籤表是災難性的。
//
// 防連點靠 UNIQUE (tournament_id, fencer_id),**不加冪等鍵**:
// 一個概念一個權威位置(鐵則 9),而資料庫的約束比應用層的先查後寫可靠。
func (s *Service) Register(ctx context.Context, p RegisterParams) (*RegisterResult, error) {
	gameID, err := cleanGameID(p.GameID)
	if err != nil {
		return nil, err
	}
	// Discord 名稱選填(2026-09-13)。空字串是合法值,照樣寫進去 ——
	// discord_name 是 NOT NULL,而 adapter 那句 NULLIF/COALESCE 會處理
	// 「沒填就別洗掉跨屆檔案上原有的那一份」。
	discordName := strings.TrimSpace(p.DiscordName)
	displayName := strings.TrimSpace(p.DisplayName)
	if displayName == "" {
		displayName = gameID
	}
	if p.SelfRatedRank != bp.RankUnspecified && !p.SelfRatedRank.Valid() {
		return nil, fmt.Errorf("%w: 自評段位 %d", tournament.ErrInvalidRank, p.SelfRatedRank)
	}
	ladderScore := p.LadderScore
	if ladderScore < 0 {
		// 論劍積分不會是負的;與其拒絕整份報名表,不如當成沒填 ——
		// 這個欄位只是評段的參考,不值得為它把一個人擋在門外。
		ladderScore = 0
	}
	ladderRank := strings.TrimSpace(p.LadderRank)
	artsNote := strings.TrimSpace(p.ArtsNote)
	availabilityNote := strings.TrimSpace(p.AvailabilityNote)
	if err := errors.Join(
		checkLen("display_name", displayName, maxDisplayNameLen),
		checkLen("discord_name", discordName, maxDiscordNameLen),
		checkLen("ladder_rank", ladderRank, maxLadderRankLen),
		checkLen("arts_note", artsNote, maxNoteLen),
		checkLen("availability_note", availabilityNote, maxNoteLen),
	); err != nil {
		return nil, err
	}

	t, err := s.repo.TournamentBySlug(ctx, p.TournamentSlug)
	if err != nil {
		return nil, err
	}
	if err := tournament.RequirePhase(t.Phase, tournament.PhaseSignup); err != nil {
		return nil, err
	}

	// 明碼產生了但**不回傳**:passcode_hash 是 NOT NULL,而與它同時寫下的
	// passcode_issued_at 是日後作廢 session 的依據。登入用不到明碼,
	// 所以它在這個函式結束時就沒有任何副本留下來(見 RegisterResult)。
	passcode, err := GeneratePasscode()
	if err != nil {
		return nil, err
	}
	hash, err := s.hasher.Hash(passcode)
	if err != nil {
		return nil, err
	}

	reg, err := s.repo.CreateRegistration(ctx, CreateRegistrationParams{
		TournamentID:     t.ID,
		RequirePhase:     tournament.PhaseSignup,
		GameID:           gameID,
		DisplayName:      displayName,
		DiscordName:      discordName,
		PasscodeHash:     hash,
		SelfRatedRank:    p.SelfRatedRank,
		LadderRank:       ladderRank,
		LadderScore:      ladderScore,
		ArtsNote:         artsNote,
		AvailabilityNote: availabilityNote,
	})
	if err != nil {
		return nil, err
	}

	return &RegisterResult{
		Player:          reg.Player,
		Fencer:          reg.Fencer,
		ReturningFencer: reg.ReturningFencer,
		PreviousRank:    reg.PreviousRank,
	}, nil
}

// LoginParams 是一次選手登入。**沒有通行碼欄位**,見 Login。
type LoginParams struct {
	TournamentSlug string
	GameID         string
}

// Login 用遊戲ID 驗明選手身分。
//
// # 沒有通行碼(2026-09-13 定案)
//
// 給對遊戲ID 就能登入。代價是已知且被接受的:遊戲ID 全服唯一且公開
// (對戰表上就印著),所以任何人知道某位選手的遊戲ID 就能以他的身分登入、
// 花掉他的 BP、改他的讓武選擇。換來的是報名到登入之間沒有任何要抄寫的東西。
//
// # 狀態是唯一的門鎖
//
// 既然沒有密碼可以換,「把某個人擋在外面」就只剩他的狀態這一個槓桿:
// **只有 PlayerActive 登得進來**,棄賽與已淘汰都不行。所以裁判的
// WithdrawPlayer 不只是賽程上的處置,它同時是「鎖住這個帳號」的手段
// (要連現在還活著的 session 一起踢掉,再做一次 RegeneratePasscode)。
//
// # 三條失敗路徑必須長得一模一樣
//
// 查無此遊戲ID、非 active、格式不合 —— 全部回同一個 ErrInvalidCredentials,
// 訊息裡沒有任何能分辨它們的線索。分得出來的話,登入頁就成了一支
// 「這個 ID 報名了沒 / 他是不是被裁判擋掉了」的查詢介面,而後者絕不該由
// 一個匿名請求問得出來。
//
// 時序上也不再有可利用的差異:三條路徑最多各做一次同樣的索引查詢,
// 沒有雜湊比對那種數量級的成本差(原本那行誘餌雜湊隨通行碼一起走了)。
// 至於「登入成功」本身洩漏了這個 ID 存在且 active —— 那正是這次決策接受的代價,
// 不是可以在這裡補起來的洞。
//
// 這裡**不發 session** —— 換發 cookie 是 transport 的事,見套件註解。
func (s *Service) Login(ctx context.Context, p LoginParams) (*tournament.Player, error) {
	// 賽事 slug 查不到是真正的 404:slug 本來就在網址列上,不是秘密。
	t, err := s.repo.TournamentBySlug(ctx, p.TournamentSlug)
	if err != nil {
		return nil, err
	}

	gameID, err := cleanGameID(p.GameID)
	if err != nil {
		// 格式不合也走同一個出口:回 ErrInvalidGameID 等於告訴對方
		// 「這串字連查都不必查」,那是一個可以拿來縮小猜測範圍的訊號。
		return nil, ErrInvalidCredentials
	}

	player, err := s.repo.PlayerByGameID(ctx, t.ID, gameID)
	if err != nil {
		if errors.Is(err, tournament.ErrPlayerNotFound) {
			return nil, ErrInvalidCredentials
		}
		// 資料庫掛了不是憑證問題。折成登入失敗的話,一次故障會表現成
		// 「全場選手的遊戲ID 突然都不對了」,而所有人重試只會讓故障更嚴重。
		return nil, err
	}
	if player.Status != tournament.PlayerActive {
		return nil, ErrInvalidCredentials
	}
	return &player, nil
}

// RegenerateParams 是裁判為某位選手重新產生通行碼。
type RegenerateParams struct {
	TournamentSlug string
	PlayerPublicID string
	// ActorUserID 是執行的裁判,必填。
	//
	// 刻意**沒有**選手自助重取這條路。理由隨著通行碼登入取消而換了一個,
	// 但結論沒變:換發現在等於「把這個人所有 session 踢掉」,而遊戲ID 是
	// 公開資訊 —— 自助等於「輸入任何人的遊戲ID 就能把他登出」。
	// 何況 admin_audit_logs.actor_user_id 是 NOT NULL,這個動作本來就要有人署名。
	ActorUserID int64
	Reason      string
}

// RegenerateResult 是重新產生的結果。
type RegenerateResult struct {
	Player tournament.Player
	// Passcode 是新的明碼通行碼。
	//
	// 登入用不到它(2026-09-13 起只要遊戲ID),所以它現在是給裁判的一張收據 ——
	// 「這個人的秘密真的被換掉了」。要不要繼續回傳由裁判端的契約決定,
	// 這一層只負責不把它寫到 return 以外的任何地方。
	Passcode string
}

// RegeneratePasscode 換發通行碼 —— 現在真正的用途是**踢掉這位選手所有的 session**。
//
// 登入不再比對通行碼,所以「舊碼立即失效」已經不是重點;起作用的是一起被推進的
// passcode_issued_at:已簽發的 token 裡帶著簽發當下的值,對不上就當場失效
// (見 internal/core/activity/session)。
//
// 它與 WithdrawPlayer 是兩道互補的槓桿:棄賽擋住**之後**的登入,
// 換發踢掉**現在**還活著的 session。要立刻把一個人完全隔離,兩個都要做。
//
// 不限階段 —— 需要把某人踢出去的時機不會挑階段,包含開賽當天。
func (s *Service) RegeneratePasscode(ctx context.Context, p RegenerateParams) (*RegenerateResult, error) {
	if p.ActorUserID == 0 {
		return nil, tournament.ErrActorRequired
	}
	t, err := s.repo.TournamentBySlug(ctx, p.TournamentSlug)
	if err != nil {
		return nil, err
	}
	player, err := s.repo.PlayerByPublicID(ctx, t.ID, p.PlayerPublicID)
	if err != nil {
		return nil, err
	}

	passcode, err := GeneratePasscode()
	if err != nil {
		return nil, err
	}
	hash, err := s.hasher.Hash(passcode)
	if err != nil {
		return nil, err
	}
	if err := s.repo.UpdatePasscode(ctx, UpdatePasscodeParams{
		TournamentID: t.ID,
		PlayerID:     player.ID,
		PasscodeHash: hash,
		ActorUserID:  p.ActorUserID,
		Reason:       p.Reason,
	}); err != nil {
		return nil, err
	}
	return &RegenerateResult{Player: player, Passcode: passcode}, nil
}

// BindAccountParams 是把活動層身分接到平台帳號。
type BindAccountParams struct {
	TournamentSlug string
	PlayerPublicID string
	// UserID 來自平台 Bearer(Discord OAuth 登入)。
	//
	// transport 必須確認呼叫者**同時**持有活動層 session 與平台 Bearer:
	// 綁定決定獎金匯到誰的帳戶,要由本人雙重證明。刻意不讓裁判代綁。
	UserID int64
}

// BindPlatformAccount 把 user_id 寫進 fencers 與 tournament_players。
//
// **領獎前必須先綁**(grill Q24)—— 獎金走 Ledger,要有收款對象。
//
// 重複綁到同一個帳號視為成功(冪等):使用者連點兩次不該看到錯誤,
// 而這件事沒有副作用可言。綁到**不同**帳號則一律拒絕,
// 改綁要裁判介入並留稽核紀錄,不能靠這支 API 悄悄換掉收款人。
func (s *Service) BindPlatformAccount(ctx context.Context, p BindAccountParams) (*tournament.Player, error) {
	if p.UserID == 0 {
		return nil, ErrUserRequired
	}
	t, err := s.repo.TournamentBySlug(ctx, p.TournamentSlug)
	if err != nil {
		return nil, err
	}
	player, err := s.repo.PlayerByPublicID(ctx, t.ID, p.PlayerPublicID)
	if err != nil {
		return nil, err
	}
	if player.Bound() {
		if *player.UserID == p.UserID {
			return &player, nil
		}
		return nil, ErrAlreadyBound
	}

	updated, err := s.repo.BindPlatformAccount(ctx, BindParams{
		TournamentID: t.ID,
		PlayerID:     player.ID,
		FencerID:     player.FencerID,
		UserID:       p.UserID,
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// cleanGameID 整理並驗證遊戲ID。
//
// 遊戲ID 是自然鍵、是登入帳號、也是對戰表上的辨識名,所以它的形狀
// 必須在進資料庫**之前**就定下來:前後空白沒清掉的話,
// 「御風羽」與「御風羽 」會是 UNIQUE (game_id) 眼中的兩個人,
// 而那兩列在畫面上長得一模一樣。
func cleanGameID(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", ErrGameIDRequired
	}
	if utf8.RuneCountInString(s) > maxGameIDLen {
		return "", fmt.Errorf("%w: 最多 %d 字", ErrInvalidGameID, maxGameIDLen)
	}
	for _, r := range s {
		// 控制字元(含換行、定位、零寬字元裡的那幾個)會讓兩個看起來一樣的
		// 遊戲ID 變成不同的鍵,是冒名的現成工具。
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: 不可含控制字元", ErrInvalidGameID)
		}
	}
	return s, nil
}

// checkLen 檢查欄位長度(以字元數計,不是位元組)。
func checkLen(field, s string, max int) error {
	if utf8.RuneCountInString(s) > max {
		return fmt.Errorf("%w: %s 最多 %d 字", ErrFieldTooLong, field, max)
	}
	return nil
}
