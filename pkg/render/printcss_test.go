package render

import "testing"

func TestSourcePageRulesAreRemovedWithoutChangingOtherCSS(t *testing.T) {
	for _, tc := range []struct{ name, source, want string }{
		{"plain", `p{color:blue}@page{size:20in;margin:0}b{color:red}`, `p{color:blue}b{color:red}`},
		{"specific", `@media print{@page receipt:first{size:20in!important;@top-center{content:"x}"}}p{color:blue}}`, `@media print{p{color:blue}}`},
		{"escaped", `@\70 age:left{size:20in}@\000070age{margin:0}p{color:blue}`, `p{color:blue}`},
		{"case", `@PAGE :right{size:20in}p{color:blue}`, `p{color:blue}`},
		{"strings-comments-functions", `/* @page {x} */p{content:"@page {x}";background:url(data:example@page);color:blue}`, `/* @page {x} */p{content:"@page {x}";background:url(data:example@page);color:blue}`},
		{"different-rule", `@pages{color:blue}@supports(display:grid){p{color:blue}}`, `@pages{color:blue}@supports(display:grid){p{color:blue}}`},
		{"escaped-string", `p{content:"\22 @page stays text"}@page{size:20in}`, `p{content:"\22 @page stays text"}`},
		{"unterminated", `p{color:blue}@page { size:20in`, `p{color:blue}`},
		{"no-block", `@page; p{color:blue}`, ` p{color:blue}`},
		{"bad-string-recovery", "p{content:\"broken\n} @page :first{size:20in}b{color:red}", "p{content:\"broken\n} b{color:red}"},
		{"bad-url-recovery", `p{background:url(bad"token)}@page :first{size:20in}b{color:red}`, `p{background:url(bad"token)}b{color:red}`},
		{"escaped-url-recovery", `p{background:u\72 l(bad"token)}@page :first{size:20in}b{color:red}`, `p{background:u\72 l(bad"token)}b{color:red}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := withoutSourcePages(tc.source); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
