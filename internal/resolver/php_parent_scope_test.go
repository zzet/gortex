package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestResolveAll_PHPParentScope(t *testing.T) {
	files := map[string]string{
		"app/Base/Job.php": `<?php
namespace Acme\Base;
abstract class Job {
    public function boot(): void {}
    public function handle(): void {}
    public function other(): void {}
    public function __construct() {}
}`,
		"app/Jobs/SendJob.php": `<?php
namespace Acme\Jobs;
use Acme\Base\Job;
class SendJob extends Job {
    public function boot(): void { parent::boot(); }
}`,
		"app/Jobs/Retry/RetryJob.php": `<?php
namespace Acme\Jobs\Retry;
use Acme\Jobs\SendJob;
class RetryJob extends SendJob {
    public function boot(): void { parent::boot(); }
    public function handle(): void { parent::handle(); }
    public function __construct() { parent::__construct(); }
}`,
		"app/Jobs/ExternalJob.php": `<?php
namespace Acme\Jobs;
use Vendor\Queue\Worker;
class ExternalJob extends Worker {
    public function boot(): void { parent::boot(); }
    public function handle(): void { parent::handle(); }
    public function __construct() { parent::__construct(); }
}`,
		"app/Jobs/OtherJob.php": `<?php
namespace Acme\Jobs;
class OtherJob {
    public function boot(): void {}
    public function handle(): void {}
}`,
		"app/Other/Base.php": `<?php
namespace Acme\Other;
class Base {
    public function boot(): void {}
}`,
		"app/Jobs/Base.php": `<?php
namespace Acme\Jobs;
class Base {
    public function boot(): void {}
}`,
		"app/Jobs/OneLine.php": `<?php
namespace Acme\Jobs;
use Acme\Other\Base;
class OneLine extends Base { public function boot(): void { parent::boot(); } }`,
		"app/Jobs/Job.php": `<?php
namespace Acme\Jobs;
class Job extends \Acme\Base\Job {
    public function boot(): void { parent::boot(); }
    public function handle(): void { parent::other(); }
    public function other(): void {}
}`,
		"app/Jobs/AliasedJob.php": `<?php
namespace Acme\Jobs;
use Acme\Base\Job as BaseJob;
class AliasedJob extends BaseJob {
    public function boot(): void { parent::boot(); }
}`,
		"app/Jobs/Retry/Job.php": `<?php
namespace Acme\Jobs\Retry;
class Job extends \Acme\Jobs\Job {
    public function boot(): void { parent::boot(); }
    public function handle(): void { parent::handle(); }
    public function __construct() { parent::__construct(); }
}`,
		"app/External/Job.php": `<?php
namespace Acme\External;
class Job extends \Acme\Missing\Job {
    public function boot(): void { parent::boot(); }
    public function handle(): void { parent::other(); }
    public function other(): void {}
}`,
		"app/Jobs/JobTrait.php": `<?php
namespace Acme\Jobs;
trait JobTrait {
    public function boot(): void { parent::boot(); }
}`,
		"app/Jobs/LocalJob.php": `<?php
namespace Acme\Jobs;
class LocalJob {
    public function boot(): void { self::boot(); }
    public function handle(): void { $this->handle(); }
    public static function run(): void { static::run(); }
}`,
		"app/Traits/Boots.php": `<?php
namespace Acme\Traits;
trait Boots {
    use Shared;
    public function boot(): void {}
    public function other(): void {}
}`,
		"app/Traits/Shared.php": `<?php
namespace Acme\Traits;
trait Shared {
    public function handle(): void {}
}`,
		"app/Other/Boots.php": `<?php
namespace Acme\Other;
trait Boots {
    public function boot(): void {}
}`,
		"app/Base/TraitJob.php": `<?php
namespace Acme\Base;
use Acme\Traits\Boots as JobBoots;
class TraitJob extends Job {
    use JobBoots;
    public function other(): void {}
}`,
		"app/Jobs/TraitJob.php": `<?php
namespace Acme\Jobs;
class TraitJob extends \Acme\Base\TraitJob {
    public function boot(): void { parent::boot(); }
    public function other(): void { parent::other(); }
}`,
		"app/Jobs/Retry/TraitJob.php": `<?php
namespace Acme\Jobs\Retry;
class TraitJob extends \Acme\Jobs\TraitJob {
    public function handle(): void { parent::handle(); }
}`,
	}
	for _, mode := range []string{"all", "same names"} {
		g := graph.New()
		for path, src := range files {
			if mode == "same names" && path != "app/Base/Job.php" && path != "app/Jobs/Job.php" && path != "app/Jobs/Retry/Job.php" {
				continue
			}
			result, err := languages.NewPHPExtractor().Extract(path, []byte(src))
			require.NoError(t, err)
			for _, n := range result.Nodes {
				g.AddNode(n)
			}
			for _, e := range result.Edges {
				g.AddEdge(e)
			}
		}

		New(g).ResolveAll()

		for _, tc := range []struct {
			name, caller, target string
		}{
			{"direct parent across directories", "app/Jobs/SendJob.php::SendJob.boot", "app/Base/Job.php::Job.boot"},
			{"nearest ancestor", "app/Jobs/Retry/RetryJob.php::RetryJob.boot", "app/Jobs/SendJob.php::SendJob.boot"},
			{"grandparent", "app/Jobs/Retry/RetryJob.php::RetryJob.handle", "app/Base/Job.php::Job.handle"},
			{"inherited constructor", "app/Jobs/Retry/RetryJob.php::RetryJob.__construct", "app/Base/Job.php::Job.__construct"},
			{"external parent", "app/Jobs/ExternalJob.php::ExternalJob.boot", ""},
			{"external parent with local decoy", "app/Jobs/ExternalJob.php::ExternalJob.handle", ""},
			{"one-line imported parent with local decoy", "app/Jobs/OneLine.php::OneLine.boot", "app/Other/Base.php::Base.boot"},
			{"qualified same-name parent", "app/Jobs/Job.php::Job.boot", "app/Base/Job.php::Job.boot"},
			{"different parent method", "app/Jobs/Job.php::Job.handle", "app/Base/Job.php::Job.other"},
			{"aliased parent", "app/Jobs/AliasedJob.php::AliasedJob.boot", "app/Base/Job.php::Job.boot"},
			{"same-name nearest ancestor", "app/Jobs/Retry/Job.php::Job.boot", "app/Jobs/Job.php::Job.boot"},
			{"same-name inherited method", "app/Jobs/Retry/Job.php::Job.handle", "app/Jobs/Job.php::Job.handle"},
			{"same-name inherited constructor", "app/Jobs/Retry/Job.php::Job.__construct", "app/Base/Job.php::Job.__construct"},
			{"same-name external parent", "app/External/Job.php::Job.boot", ""},
			{"external different method", "app/External/Job.php::Job.handle", ""},
			{"trait parent needs using class", "app/Jobs/JobTrait.php::JobTrait.boot", ""},
			{"self unchanged", "app/Jobs/LocalJob.php::LocalJob.boot", "app/Jobs/LocalJob.php::LocalJob.boot"},
			{"this unchanged", "app/Jobs/LocalJob.php::LocalJob.handle", "app/Jobs/LocalJob.php::LocalJob.handle"},
			{"static unchanged", "app/Jobs/LocalJob.php::LocalJob.run", "app/Jobs/LocalJob.php::LocalJob.run"},
			{"parent trait before grandparent method", "app/Jobs/TraitJob.php::TraitJob.boot", "app/Traits/Boots.php::Boots.boot"},
			{"grandparent nested trait", "app/Jobs/Retry/TraitJob.php::TraitJob.handle", "app/Traits/Shared.php::Shared.handle"},
			{"parent method before trait", "app/Jobs/TraitJob.php::TraitJob.other", "app/Base/TraitJob.php::TraitJob.other"},
		} {
			if mode == "same names" && g.GetNode(tc.caller) == nil {
				continue
			}
			t.Run(tc.name, func(t *testing.T) {
				var calls []*graph.Edge
				for _, e := range g.GetOutEdges(tc.caller) {
					if e.Kind == graph.EdgeCalls {
						calls = append(calls, e)
					}
				}
				require.Len(t, calls, 1)
				if tc.target == "" {
					assert.True(t, graph.IsUnresolvedTarget(calls[0].To), "parent call must stay unresolved, got %s", calls[0].To)
				} else {
					assert.Equal(t, tc.target, calls[0].To)
				}
			})
		}
	}
}
