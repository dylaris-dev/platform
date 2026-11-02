package handlers

import (
	"fmt"

	"dylaris-core/models"
)

var ramPaddingRefused = fmt.Sprintf("ramPaddingMb must be a whole number of MB from 0 to %d", models.MaxRAMPaddingMB)
