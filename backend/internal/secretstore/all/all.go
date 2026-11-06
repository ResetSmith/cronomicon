// Package all registers every secret store provider. Import it (blank) from
// any package that looks a provider up.
package all

import (
	_ "github.com/ResetSmith/cronomicon/internal/secretstore/hashicorp"
)
