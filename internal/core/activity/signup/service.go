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
// 明碼只存在於兩個回傳值裡:Register 與 RegeneratePasscode。
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
// 除了前三欄,其餘全是**給裁判評段用的參考資料** —— 評段要綜合論劍段位、
// 積分、實戰經驗與整體 PVP 實力,所以表單問的比「你叫什麼」多。
type RegisterParams struct {
	TournamentSlug string
	// GameID 全服唯一。報過往屆的話會自動接上既有的選手檔案。
	GameID string
	// DisplayName 留空則沿用 GameID。
	DisplayName string
	DiscordName string
	// SelfRatedRank 自評段位;RankUnspecified = 沒填。
	// 除了當評段起點,也讓裁判看得出誰高估或低估自己。
	SelfRatedRank    bp.Rank
	LadderRank       string
	LadderScore      int32
	ArtsNote         string
	AvailabilityNote string
}

// RegisterResult 帶回**唯一一次**看得到明碼通行碼的機會。
type RegisterResult struct {
	Player tournament.Player
	Fencer Fencer
	// Passcode 是明碼通行碼。只在這裡出現一次,資料庫只存 hash,
	// 裁判後台也看不到。前端必須明確提示使用者抄下來。
	Passcode string
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
	discordName := strings.TrimSpace(p.DiscordName)
	if discordName == "" {
		return nil, ErrDiscordNameRequired
	}
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
		Passcode:        passcode,
		ReturningFencer: reg.ReturningFencer,
		PreviousRank:    reg.PreviousRank,
	}, nil
}

// LoginParams 是一次選手登入。
type LoginParams struct {
	TournamentSlug string
	GameID         string
	Passcode       string
}

// Login 用遊戲ID + 通行碼驗明選手身分。
//
// # 兩條失敗路徑必須長得一模一樣
//
// 「查無此遊戲ID」與「通行碼錯誤」回同一個 ErrInvalidCredentials,
// 訊息裡沒有任何能分辨兩者的線索(schemas/20 明訂)。否則登入頁就成了
// 一份「誰報了名」的查詢介面,而報名期間那不是公開資訊。
//
// 光是回同一個錯誤還不夠:查無此人時若直接返回,它會比「查到人但驗證失敗」
// 快上幾十毫秒,而那個時間差一樣答得出「這個 ID 存不存在」。
// 所以查無此人時照樣跑一次完整的雜湊比對(VerifyDummy),把兩條路徑的
// 工作量拉平。這是本函式唯一看起來「多餘」的一行,不要刪。
//
// 這裡**不發 session** —— 換發 cookie 是 transport 的事,見套件註解。
func (s *Service) Login(ctx context.Context, p LoginParams) (*tournament.Player, error) {
	gameID := strings.TrimSpace(p.GameID)
	passcode := NormalizePasscode(p.Passcode)

	// 賽事 slug 查不到是真正的 404:slug 本來就在網址列上,不是秘密。
	t, err := s.repo.TournamentBySlug(ctx, p.TournamentSlug)
	if err != nil {
		return nil, err
	}

	if gameID == "" {
		s.hasher.VerifyDummy(passcode)
		return nil, ErrInvalidCredentials
	}

	cred, err := s.repo.CredentialByGameID(ctx, t.ID, gameID)
	if err != nil {
		if errors.Is(err, tournament.ErrPlayerNotFound) {
			s.hasher.VerifyDummy(passcode)
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}

	ok, err := s.hasher.Verify(cred.PasscodeHash, passcode)
	if err != nil {
		// 存下來的雜湊壞了是系統問題,但**不能**讓使用者知道 ——
		// 「這個 ID 的雜湊格式不對」等於承認這個 ID 存在。
		// 對外仍是同一個錯誤;現場的症狀是這位選手永遠登不進去,
		// 裁判用「重新產生通行碼」就能修好。
		return nil, ErrInvalidCredentials
	}
	if !ok {
		return nil, ErrInvalidCredentials
	}
	return &cred.Player, nil
}

// RegenerateParams 是裁判為某位選手重新產生通行碼。
type RegenerateParams struct {
	TournamentSlug string
	PlayerPublicID string
	// ActorUserID 是執行的裁判,必填。
	//
	// 刻意**沒有**選手自助重取這條路:遊戲ID 是公開資訊(對戰表上就有),
	// 自助重取等於「輸入任何人的遊戲ID 就能把他的通行碼換掉」——
	// 那是一鍵帳號接管,而且原持有人只會覺得自己抄錯了碼。
	ActorUserID int64
	Reason      string
}

// RegenerateResult 是重新產生的結果。
type RegenerateResult struct {
	Player tournament.Player
	// Passcode 是新的明碼通行碼,同樣只出現這一次。
	Passcode string
}

// RegeneratePasscode 換發通行碼,舊碼立即失效。
//
// 「立即失效」不需要額外的撤銷機制:passcode_hash 被覆寫的那一刻,
// 舊碼就再也比對不過。少一張撤銷清單就少一個會忘記清的地方。
//
// 不限階段 —— 選手可能在任何時候把碼弄丟,包含開賽當天。
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
