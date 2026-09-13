package main

import (
	_ "time/tzdata"

	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	_ "github.com/caddyserver/caddy/v2/modules/standard"
	_ "github.com/dariopb/goreverselb/caddy"
	_ "github.com/mholt/caddy-l4"
)

func main() {
	caddycmd.Main()
}
