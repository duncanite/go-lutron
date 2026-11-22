package leap_test

import (
	"errors"
	"slices"
	"strconv"
	"testing"

	"github.com/duncanite/go-lutron/leap"
	"github.com/duncanite/go-lutron/leap/leaptest"
)

// page is an answer to request under status, its header carrying paging
// when there is one, and its body.
func page(request leap.Message, status, paging, body string) string {
	header := `"ClientTag":"` + request.Header.ClientTag + `","StatusCode":"` + status + `"`
	if paging != "" {
		header += `,"Paging":` + paging
	}

	return `{"CommuniqueType":"ReadResponse","Header":{` + header + `},"Body":` + body + "}\r\n"
}

// more is a Paging that says more follows.
const more = `{"ContinuationToken":"more"}`

// TestPages: an answer in pages is asked for page by page, each request
// carrying the Paging of the answer before, until an answer carries none;
// a refusal ends the pages with its answer, and so does an answer in more
// pages than one may be, with none; nothing is asked past the last page,
// nor once the caller stops.
func TestPages(t *testing.T) {
	t.Parallel()

	t.Run("each page is asked for with the Paging of the one before", func(t *testing.T) {
		t.Parallel()

		conn, read, write := piped(t)
		asked := make(chan leap.Message, 3)

		go func() {
			for index, paging := range []string{
				`{"ContinuationToken":"one"}`, `{ "ContinuationToken": "two", "Left": [1, 2] }`, "",
			} {
				request := read()
				asked <- request

				write(page(request, "200 OK", paging, `{"n":`+strconv.Itoa(index)+`}`))
			}
		}()

		var bodies []string

		for answer, err := range conn.Pages(t.Context(), leap.Read, "/area", map[string]int{"x": 1}) {
			if err != nil {
				t.Fatal(err)
			}

			bodies = append(bodies, string(answer.Body))
		}

		if want := []string{`{"n":0}`, `{"n":1}`, `{"n":2}`}; !slices.Equal(bodies, want) {
			t.Fatalf("the pages = %q, want %q", bodies, want)
		}

		tags := map[string]bool{}

		for _, paging := range []string{"", `{"ContinuationToken":"one"}`, `{"ContinuationToken":"two","Left":[1,2]}`} {
			request := <-asked
			tags[request.Header.ClientTag] = true

			if request.CommuniqueType != leap.Read || request.Header.URL != "/area" ||
				string(request.Body) != `{"x":1}` || string(request.Header.Paging) != paging {
				t.Errorf("a page was asked for with %s %s, body %s, Paging %q; want Paging %q",
					request.CommuniqueType, request.Header.URL, request.Body, request.Header.Paging, paging)
			}
		}

		if len(tags) != 3 {
			t.Errorf("three pages were asked for under %d tags", len(tags))
		}
	})

	for _, last := range []string{"", "null", "{}", "{ }"} {
		t.Run("the last page carries the Paging "+strconv.Quote(last), func(t *testing.T) {
			t.Parallel()

			conn, read, write := piped(t)
			asked := make(chan int, 1)

			go func() {
				paging, count := last, 0

				for {
					request := read()
					if request.Header.URL != "/area" {
						asked <- count

						write(page(request, "200 OK", "", `{}`))

						return
					}

					count++

					write(page(request, "200 OK", paging, `{}`))

					paging = ""
				}
			}()

			pages := 0

			for _, err := range conn.Pages(t.Context(), leap.Read, "/area", nil) {
				if err != nil {
					t.Fatal(err)
				}

				pages++
			}

			if _, err := conn.Request(t.Context(), leap.Read, "/after", nil); err != nil {
				t.Fatal(err)
			}

			if count := <-asked; pages != 1 || count != 1 {
				t.Fatalf("%d pages handed over and %d asked for; want one, and nothing asked after it", pages, count)
			}
		})
	}

	t.Run("a page refused ends them, with its answer", func(t *testing.T) {
		t.Parallel()

		conn, read, write := piped(t)
		after := make(chan string, 1)

		go func() {
			write(page(read(), "200 OK", more, `{}`))
			write(page(read(), "400 BadRequest", more, `{}`))

			next := read()
			after <- next.Header.URL

			write(page(next, "200 OK", "", `{}`))
		}()

		var (
			pages   int
			refusal *leap.Message
			failed  error
		)

		for answer, err := range conn.Pages(t.Context(), leap.Read, "/area", nil) {
			pages++
			refusal, failed = answer, err
		}

		if pages != 2 || !errors.Is(failed, leap.ErrStatus) || refusal == nil ||
			refusal.Header.StatusCode != "400 BadRequest" {
			t.Fatalf("%d pages, the last %+v with %v; want two, the second the refusal", pages, refusal, failed)
		}

		if _, err := conn.Request(t.Context(), leap.Read, "/after", nil); err != nil {
			t.Fatal(err)
		}

		if next := <-after; next != "/after" {
			t.Fatalf("after a page was refused, %s was asked for", next)
		}
	})

	// endless answers every request for /area with a page that says more
	// follows, and tells how many it answered once /after is asked for.
	endless := func(t *testing.T) (*leap.Conn, chan int) {
		t.Helper()

		conn, read, write := piped(t)
		answered := make(chan int, 1)

		go func() {
			count := 0

			for {
				request := read()
				if request.Header.URL != "/area" {
					answered <- count

					write(page(request, "200 OK", "", `{}`))

					return
				}

				count++

				write(page(request, "200 OK", more, `{}`))
			}
		}()

		return conn, answered
	}

	t.Run("an answer in more pages than one may be", func(t *testing.T) {
		t.Parallel()

		conn, answered := endless(t)

		var (
			pages  int
			last   *leap.Message
			failed error
		)

		for answer, err := range conn.Pages(t.Context(), leap.Read, "/area", nil) {
			pages++
			last, failed = answer, err
		}

		if _, err := conn.Request(t.Context(), leap.Read, "/after", nil); err != nil {
			t.Fatal(err)
		}

		if asked := <-answered; asked != 256 || pages != 257 || last != nil || !errors.Is(failed, leap.ErrMessage) {
			t.Fatalf("%d pages asked for, %d handed over, the last %+v with %v; want 256, then ErrMessage alone",
				asked, pages, last, failed)
		}
	})

	t.Run("nothing is asked once the caller stops", func(t *testing.T) {
		t.Parallel()

		conn, answered := endless(t)

		for _, err := range conn.Pages(t.Context(), leap.Read, "/area", nil) {
			if err != nil {
				t.Fatal(err)
			}

			break
		}

		if _, err := conn.Request(t.Context(), leap.Read, "/after", nil); err != nil {
			t.Fatal(err)
		}

		if asked := <-answered; asked != 1 {
			t.Fatalf("a caller that stopped at the first page had %d asked for", asked)
		}
	})
}

// TestPagesOfTheStandIn: the stand-in told to page answers a list a few
// at a time, in order, and whole again when told to stop; what is not a
// list comes whole.
func TestPagesOfTheStandIn(t *testing.T) {
	t.Parallel()

	bridge := leaptest.New()
	t.Cleanup(bridge.Close)

	bridge.Set("/area", "MultipleAreaDefinition", map[string]any{"Areas": []int{1, 2, 3, 4, 5}})

	conn := dial(t, bridge, bridge.Credentials())

	read := func(url string) []string {
		t.Helper()

		var bodies []string

		for answer, err := range conn.Pages(t.Context(), leap.Read, url, nil) {
			if err != nil {
				t.Fatalf("a page of %s: %v", url, err)
			}

			bodies = append(bodies, string(answer.Body))
		}

		return bodies
	}

	bridge.Page(2)

	if got, want := read(
		"/area",
	), []string{
		`{"Areas":[1,2]}`,
		`{"Areas":[3,4]}`,
		`{"Areas":[5]}`,
	}; !slices.Equal(
		got,
		want,
	) {
		t.Errorf("five areas, two at a time = %q, want %q", got, want)
	}

	if got := read(leaptest.PingURL); len(got) != 1 {
		t.Errorf("what is no list came in %d pages", len(got))
	}

	bridge.Page(5)

	if got, want := read("/area"), []string{`{"Areas":[1,2,3,4,5]}`}; !slices.Equal(got, want) {
		t.Errorf("five areas, five at a time = %q, want %q", got, want)
	}

	bridge.Page(0)

	if got, want := read("/area"), []string{`{"Areas":[1,2,3,4,5]}`}; !slices.Equal(got, want) {
		t.Errorf("five areas, whole = %q, want %q", got, want)
	}
}
