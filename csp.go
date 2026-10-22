package main

// Content-Security-Policy for the PWA's own pages (run-bang-phase2-review.md §2 "XSS inside the
// PWA", §5 #7). Session output is untrusted and the app builds DOM with innerHTML in many places;
// the policy makes an injected string inert as script: no inline script, no eval, no foreign
// origin to load from or send to. Styles keep 'unsafe-inline' (the app's large inline <style>
// blocks and style attributes): a style injection cannot run code or reach the network
// (img-src/connect-src stay 'self').

import (
	"net/http"
	"regexp"
)

var cspHostRe = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)

// cspPolicy is the header for a page served to host. connect-src names the page's own wss/ws
// origin explicitly as well as 'self', for browsers whose 'self' does not cover WebSockets.
// vault is the Vault origin the arm sheet signs through (--vault-url, run-bang-vault-sign §2.4
// step 2): exactly that one https origin joins connect-src, nothing else; "" adds nothing.
func cspPolicy(host, vault string) string {
	connect := "'self'"
	if cspHostRe.MatchString(host) {
		connect += " wss://" + host + " ws://" + host
	}
	if vault != "" {
		connect += " " + vault
	}
	return "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
		"connect-src " + connect + "; worker-src 'self'; manifest-src 'self'; font-src 'self'; object-src 'none'; " +
		"base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
}

// withCSP sets the policy on every static response (the API's JSON needs none). vault is
// vaultOrigin's result, already validated.
func withCSP(next http.Handler, vault string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", cspPolicy(r.Host, vault))
		next.ServeHTTP(w, r)
	})
}
