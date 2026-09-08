// Package authz 是管理端授權的契約層(schemas/03-authz.md)。
//
// 兩件事在這裡定案,實作(authzpg)只負責查資料庫:
//
//  1. **權限常數寫在程式碼**。schema 刻意不做「UI 可編輯的 permission 表」——
//     權限能在後台自由改本身就是風險。DB 只存「角色 ↔ 權限字串」的對應
//     (migration 00014 seed 的 21 組),字串本身的權威在這個檔案。
//  2. **procedure → 權限的映射**。RPC 全名是入口層的概念,但「哪個 RPC 需要
//     哪個權限」是領域決策,放 core 才能被 transport 與實作同時引用而不繞道。
//
// 未登記的 procedure 一律視為「拒絕」(fail closed)。漏登記要變成拒絕而不是
// 放行——這與 transport 用服務前綴判定「需不需要授權」是同一個方向的取捨。
package authz

import (
	"context"
	"errors"
)

// Permission 是權限字串。取值只能是本檔案宣告的常數;
// 這些字串必須與 migration 00014 seed 寫進 platform.role_permissions 的完全一致,
// 任何一邊改動都要同步(sql 那側靠 authzpg 的 seed 對照測試把關)。
type Permission string

// migration 00014 seed 的全部權限字串(9 個,對應 21 組角色權限)。
// 這裡列齊而不是只列目前用得到的,是為了讓「seed 有、程式碼沒有」這種漂移
// 一眼看得出來;尚未有 RPC 對應的常數是刻意保留的。
const (
	// PermEconomyGrant 是管理端動錢(發點 / 扣點)。
	PermEconomyGrant Permission = "economy.grant"
	// PermEconomyRefund 是管理端萬能退款。
	PermEconomyRefund Permission = "economy.refund"
	// PermEconomyConfig 是修改 economy_configs(尚無對應 RPC)。
	PermEconomyConfig Permission = "economy.config"
	// PermShopManage 是商品上下架與設定(尚無對應 RPC)。
	PermShopManage Permission = "shop.manage"
	// PermRedemptionHandle 是處理兌換工單。
	PermRedemptionHandle Permission = "redemption.handle"
	// PermUserRestrict 是停權三級的授予與解除(尚無對應 RPC)。
	PermUserRestrict Permission = "user.restrict"
	// PermRolesManage 是角色與授權的管理(尚無對應 RPC)。
	PermRolesManage Permission = "roles.manage"
	// PermLogsRead 是讀取管理端紀錄(含帳本分錄列表)。
	PermLogsRead Permission = "logs.read"
	// PermLogsReadDeleted 是讀取已刪除訊息(動作本身也要進 admin_audit_logs;
	// 尚無對應 RPC)。
	PermLogsReadDeleted Permission = "logs.read_deleted"
)

// AdminEconomyService 五個 RPC 的全名。
//
// 刻意手寫字串而不 import gen/…/platformv1connect 的常數:core 是契約層,
// 只認標準庫(depguard 把關),不該為了五個字串把 connect 拉進來。
// 兩邊不同步的風險用「啟動時斷言」補:cmd/server 可以拿
// ProcedurePermissions() 與 proto descriptor 對照,漏登記直接開不起來。
const (
	ProcAdminEconomyGrant            = "/hestia.platform.v1.AdminEconomyService/Grant"
	ProcAdminEconomyDeduct           = "/hestia.platform.v1.AdminEconomyService/Deduct"
	ProcAdminEconomyRefund           = "/hestia.platform.v1.AdminEconomyService/Refund"
	ProcAdminEconomyHandleRedemption = "/hestia.platform.v1.AdminEconomyService/HandleRedemption"
	ProcAdminEconomyListEntries      = "/hestia.platform.v1.AdminEconomyService/ListEntries"
)

// procedurePermissions 是 procedure → 所需權限的**唯一**映射表。
//
// 為什麼 Deduct 也是 economy.grant:seed 沒有 economy.deduct,發點與扣點在
// 角色設計上是同一種能力(能發就能扣)。要拆成兩種權限是 schema 決策,
// 不在程式碼這一側自行發明字串。
var procedurePermissions = map[string]Permission{
	ProcAdminEconomyGrant:            PermEconomyGrant,
	ProcAdminEconomyDeduct:           PermEconomyGrant,
	ProcAdminEconomyRefund:           PermEconomyRefund,
	ProcAdminEconomyHandleRedemption: PermRedemptionHandle,
	ProcAdminEconomyListEntries:      PermLogsRead,
}

// PermissionFor 回傳 procedure 所需的權限。第二個回傳值為 false 表示
// **未登記**——呼叫端必須當成拒絕,不能當成「不需要權限」。
func PermissionFor(procedure string) (Permission, bool) {
	p, ok := procedurePermissions[procedure]
	return p, ok
}

// ProcedurePermissions 回傳映射表的副本,供啟動時斷言覆蓋率
// (例:走一遍 proto descriptor,確認每個管理端 RPC 都在表裡)。
// 回副本而不是原表:啟動檢查不該有能力改動授權規則。
func ProcedurePermissions() map[string]Permission {
	out := make(map[string]Permission, len(procedurePermissions))
	for k, v := range procedurePermissions {
		out[k] = v
	}
	return out
}

// ErrPermissionDenied 是「身分有效但無權執行」的統一訊號。
//
// 訊息與 transport.ErrPermissionDenied 一致是刻意的:core 不能 import
// transport(depguard),所以 sentinel 只能定義在這裡,而入口層那個變數
// 應該收斂成 `ErrPermissionDenied = authz.ErrPermissionDenied` 的別名
// ——同一個概念只留一個權威。在收斂之前,errmap.go 需要一列
// {authz.ErrPermissionDenied, connect.CodePermissionDenied},否則授權失敗
// 會被當成未知錯誤映射成 Internal。
//
// 拒絕原因(沒有角色 / 角色過期 / 被停權 / procedure 未登記)刻意不分流:
// 對外只說「沒有權限」,細節留在 log 與 event_logs,不當成探測管道。
var ErrPermissionDenied = errors.New("沒有權限")

// Authorizer 判定已認證的使用者能不能執行某個 procedure。
// 簽名與 transport.Authorizer 完全一致(Go 介面是結構比對),
// 實作放 infrastructure/storage/authzpg,由 main 注入。
type Authorizer interface {
	Authorize(ctx context.Context, userID int64, procedure string) error
}
