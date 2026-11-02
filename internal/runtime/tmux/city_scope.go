package tmux

// IsCityScoped reports whether the configuration explicitly selects this city's
// named socket. Default, shared, and unknown sockets cannot prove city isolation.
func (c Config) IsCityScoped(cityName string) bool {
	return cityName != "" && cityName != "default" && c.SocketName == cityName
}
