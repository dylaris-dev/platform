package reqctx

import "time"

// TabProxyTicketTTL is the lifetime of a tab-proxy-scoped ticket (Claims.Purpose
// == "tab_proxy"). Short-lived because it lives in a cookie for the duration of
// one dashboard-tab iframe session, not a general 24h session token.
const TabProxyTicketTTL = 5 * time.Minute
