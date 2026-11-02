package services

import "dylaris-core/store"

// SettingMaxScheduledTasks caps the scheduled tasks one server may hold.
const SettingMaxScheduledTasks = "srv.max_scheduled_tasks"

// DefaultMaxScheduledTasks is the product default: enough for a nightly restart
// and a good number of announcements, few enough that no single server can take
// over the executor's batch.
var DefaultMaxScheduledTasks = LimitPtr(25)

// ScheduledTaskLimit is the operator's cap on tasks per server, read through
// the shared parser so the platform limit convention holds: nil means no cap at
// all, 0 means none. A settings read that fails answers with the product
// default rather than with "no cap".
func ScheduledTaskLimit(st store.Store) *int64 {
	if st == nil {
		return DefaultMaxScheduledTasks
	}
	val, err := st.GetSetting(SettingMaxScheduledTasks)
	if err != nil {
		return DefaultMaxScheduledTasks
	}
	return ParseLimitSetting(val, DefaultMaxScheduledTasks)
}
