package api

// ErrorMappers are the four services' error mappers, for the test that checks they agree.
var ErrorMappers = map[string]func(error) error{
	"agents": agentsError, "secrets": secretsError, "sessions": sessionError, "browsers": browserError,
}
