package workers

import (
	"go.mongodb.org/mongo-driver/mongo/options"
)

// optionsUpsert returns MongoDB upsert options.
func optionsUpsert() *options.UpdateOptions {
	t := true
	return &options.UpdateOptions{Upsert: &t}
}
