package migrator

import (
	"strconv"
	"strings"
)

// Latest 是通配目标版本，表示升级到已注册的最高版本。
const Latest Version = "latest"

// CompareVersions 按语义化版本规则比较两个版本号：
// 返回 -1（a<b）、0（相等）、1（a>b）。
// 仅比较主.次.补丁三段数字；预发布后缀（-rc1 等）按后缀字典序比较，
// 无后缀的正式版大于同号预发布版。无法解析的段按 0 处理。
func CompareVersions(a, b Version) int {
	majA, minA, patA, preA := parseSemVer(string(a))
	majB, minB, patB, preB := parseSemVer(string(b))
	if majA != majB {
		return intCmp(majA, majB)
	}
	if minA != minB {
		return intCmp(minA, minB)
	}
	if patA != patB {
		return intCmp(patA, patB)
	}
	switch {
	case preA == "" && preB == "":
		return 0
	case preA == "":
		return 1 // 正式版 > 预发布版
	case preB == "":
		return -1
	default:
		return strings.Compare(preA, preB)
	}
}

func parseSemVer(v string) (major, minor, patch int, pre string) {
	if i := strings.IndexByte(v, '-'); i >= 0 {
		pre = v[i+1:]
		v = v[:i]
	}
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i] // 忽略构建元数据
	}
	parts := strings.Split(v, ".")
	nums := []int{0, 0, 0}
	for i := 0; i < len(parts) && i < 3; i++ {
		n, err := strconv.Atoi(strings.TrimSpace(parts[i]))
		if err == nil {
			nums[i] = n
		}
	}
	return nums[0], nums[1], nums[2], pre
}

func intCmp(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
