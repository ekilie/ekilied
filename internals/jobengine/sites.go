package jobengine

import (
	"context"
	"fmt"
)

// createSiteDir creates the site directory.
func createSiteDir(ctx context.Context, siteName string) error {
	return run(ctx, "mkdir", "-p", fmt.Sprintf("/opt/ekilie/sites/%s", siteName))
}

// removeSiteDir removes the site directory and all its contents.
func removeSiteDir(ctx context.Context, siteName string) error {
	return run(ctx, "rm", "-rf", fmt.Sprintf("/opt/ekilie/sites/%s", siteName))
}
