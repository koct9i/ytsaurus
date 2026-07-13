#pragma once

#include "public.h"

#include <yt/yt/core/yson/public.h>

namespace NYT::NTCMalloc {

////////////////////////////////////////////////////////////////////////////////

class TTCMallocManager
{
public:
    static void Configure(const TTCMallocConfigPtr& config);
};

NYson::TYsonProducer GetTCMallocStatisticsProducer();

////////////////////////////////////////////////////////////////////////////////

} // namespace NYT::NTCMalloc
