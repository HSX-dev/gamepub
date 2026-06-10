// Package rbac 运营后台角色与权限定义。
//
// 权限点按“资源:动作”命名，角色到权限的映射在代码中静态声明并经过评审，
// 避免把“是否是管理员”散落在各个 handler 里各自判断。
package rbac

import "slices"

// 角色。
const (
	RolePlayer  = "player"
	RoleCS      = "cs"      // 客服：只读订单
	RoleOps     = "ops"     // 运营：游戏接入配置
	RoleFinance = "finance" // 财务：资金调整与对账
	RoleAdmin   = "admin"
)

// 权限点。
const (
	PermOrderRead    = "order:read"
	PermGameWrite    = "game:write"
	PermWalletRead   = "wallet:read"
	PermWalletAdjust = "wallet:adjust"
)

var rolePerms = map[string][]string{
	RoleCS:      {PermOrderRead},
	RoleOps:     {PermOrderRead, PermGameWrite},
	RoleFinance: {PermOrderRead, PermWalletRead, PermWalletAdjust},
	RoleAdmin:   {PermOrderRead, PermGameWrite, PermWalletRead, PermWalletAdjust},
}

// ValidRole 判断角色是否存在。
func ValidRole(role string) bool {
	if role == RolePlayer {
		return true
	}
	_, ok := rolePerms[role]
	return ok
}

// IsStaff 是否为后台角色（任一非玩家角色）。
func IsStaff(roles []string) bool {
	for _, r := range roles {
		if _, ok := rolePerms[r]; ok {
			return true
		}
	}
	return false
}

// HasPerm 判断角色集合是否拥有权限点。
func HasPerm(roles []string, perm string) bool {
	for _, r := range roles {
		if slices.Contains(rolePerms[r], perm) {
			return true
		}
	}
	return false
}
