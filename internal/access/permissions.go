// Package access 判定使用者可用的來源頻道：被設為來源 ∧ 使用者是該伺服器成員 ∧ 對該頻道有 View Channel 與 Read Message History。
package access

import "github.com/bwmarrin/discordgo"

// ChannelPermissions 計算 userID（擁有 roles 這些角色）在頻道 ch 的權限。
// 移植自 discordgo v0.29.0 未匯出的 memberPermissions（restapi.go:514）：
//   - 伺服器擁有者 → 全部權限；
//   - 基本權限 = @everyone 角色（ID 與伺服器相同）∪ 成員各角色的權限；含 Administrator → 全部權限（不受頻道覆寫影響）；
//   - 依序套用頻道覆寫：@everyone 覆寫 → 成員所有角色覆寫（先合併全部 deny 移除，再合併全部 allow 加上）→ 成員覆寫。
//
// 與 discordgo 的差異：Administrator 直接回傳全部權限（Discord 文件的算法），discordgo 先套用覆寫再補回頻道權限，結果相同。
// g 或 ch 為 nil 時回 0。呼叫端讀取 gateway state 中的物件時須持有 state 讀鎖。
func ChannelPermissions(g *discordgo.Guild, ch *discordgo.Channel, userID string, roles []string) int64 {
	if g == nil || ch == nil {
		return 0
	}
	if userID == g.OwnerID {
		return discordgo.PermissionAll
	}

	member := make(map[string]bool, len(roles))
	for _, r := range roles {
		member[r] = true
	}

	var perms int64
	for _, role := range g.Roles {
		if role != nil && (role.ID == g.ID || member[role.ID]) {
			perms |= role.Permissions
		}
	}
	if perms&discordgo.PermissionAdministrator != 0 {
		return discordgo.PermissionAll
	}

	for _, ow := range ch.PermissionOverwrites {
		if ow != nil && ow.Type == discordgo.PermissionOverwriteTypeRole && ow.ID == g.ID {
			perms &^= ow.Deny
			perms |= ow.Allow
			break
		}
	}

	var deny, allow int64
	for _, ow := range ch.PermissionOverwrites {
		if ow != nil && ow.Type == discordgo.PermissionOverwriteTypeRole && ow.ID != g.ID && member[ow.ID] {
			deny |= ow.Deny
			allow |= ow.Allow
		}
	}
	perms &^= deny
	perms |= allow

	for _, ow := range ch.PermissionOverwrites {
		if ow != nil && ow.Type == discordgo.PermissionOverwriteTypeMember && ow.ID == userID {
			perms &^= ow.Deny
			perms |= ow.Allow
			break
		}
	}
	return perms
}
