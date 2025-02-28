#include <library/cpp/random_provider/random_provider.h>
#include <yt/yql/providers/yt/fmr/worker/interface/yql_yt_worker.h>

namespace NYql {

struct TFmrWorkerSettings {
    TString WorkerId;
    TIntrusivePtr<IRandomProvider> RandomProvider;
    TDuration TimeToSleepBetweenRequests = TDuration::Seconds(1);
};

IFmrWorker::TPtr MakeFmrWorker(IFmrCoordinator::TPtr coordinator,IFmrJobFactory::TPtr jobFactory, const TFmrWorkerSettings& settings);

} // namspace NYql
