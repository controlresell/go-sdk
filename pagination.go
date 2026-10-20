package controlresell

import (
	"iter"
)

// paginate yields every element of a list paginated by cursor, page after page, as it is ranged over. fetch gets the
// cursor of the page to read ("" for the first one) and must send the same filters each time; the cursor is opaque.
// A failed page is yielded as the error, and ends the iteration.
func paginate[T any](fetch func(cursor string) ([]T, *string, error)) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		cursor := ""
		for {
			data, next, err := fetch(cursor)
			if err != nil {
				var zero T
				yield(zero, err)
				return
			}
			for _, element := range data {
				if !yield(element, nil) {
					return
				}
			}
			if next == nil || *next == "" {
				return
			}
			cursor = *next
		}
	}
}
