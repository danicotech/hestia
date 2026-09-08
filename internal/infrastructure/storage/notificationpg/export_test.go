package notificationpg

// MintEventIDForTest 讓外部測試拿到「本服務會發出去的」對外識別子。
//
// 為什麼要開這個縫:識別子刻意是不透明且金鑰化的,測試沒有別的辦法造出一個
// 合法值 —— 而「合法值以外一律無效」正是要驗的性質。放在 export_test.go
// 表示它只存在於測試 binary 裡,正式碼與其他套件都拿不到。
func MintEventIDForTest(s *Service, id int64) string {
	return mintEventID(s.eventID, id)
}
