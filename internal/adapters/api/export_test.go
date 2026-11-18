package api

// ErrorMappers are each service's error mappers, for the test that checks they agree.
var ErrorMappers = map[string]func(error) error{
	"agents": agentsError, "secrets": secretsError, "sessions": sessionError, "browsers": browserError,
	"needs": needsError,
}
