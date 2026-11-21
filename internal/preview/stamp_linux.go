package preview

import "syscall"

func changedAt(stat *syscall.Stat_t) int64 {
	return stat.Ctim.Nano()
}
