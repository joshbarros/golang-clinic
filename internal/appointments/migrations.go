package appointments

import "embed"

// Migrations are this module's SQL files, applied by the migrate step in appointment.
//
//go:embed migrations/*.sql
var Migrations embed.FS
