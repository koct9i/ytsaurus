#include "user_job_io_factory.h"

#include "job_spec_helper.h"
#include "helpers.h"

#include <yt/yt/ytlib/chunk_client/client_block_cache.h>
#include <yt/yt/ytlib/chunk_client/chunk_reader_host.h>
#include <yt/yt/ytlib/chunk_client/chunk_reader_options.h>
#include <yt/yt/ytlib/chunk_client/data_slice_descriptor.h>
#include <yt/yt/ytlib/chunk_client/data_source.h>
#include <yt/yt/ytlib/chunk_client/dispatcher.h>
#include <yt/yt/ytlib/chunk_client/parallel_reader_memory_manager.h>

#include <yt/yt/ytlib/controller_agent/proto/job.pb.h>

#include <yt/yt/ytlib/table_client/config.h>
#include <yt/yt/ytlib/table_client/partitioner.h>
#include <yt/yt/ytlib/table_client/partition_sort_reader.h>
#include <yt/yt/ytlib/table_client/schemaless_multi_chunk_reader.h>
#include <yt/yt/ytlib/table_client/schemaless_chunk_writer.h>
#include <yt/yt/ytlib/table_client/sorted_merging_reader.h>

#include <yt/yt/client/api/public.h>

#include <yt/yt_proto/yt/client/chunk_client/proto/chunk_meta.pb.h>
#include <yt/yt_proto/yt/client/chunk_client/proto/chunk_spec.pb.h>

#include <yt/yt/client/object_client/helpers.h>

#include <yt/yt/client/table_client/column_sort_schema.h>
#include <yt/yt/client/table_client/name_table.h>

#include <yt/yt/core/misc/protobuf_helpers.h>
#include <yt/yt/core/ytree/convert.h>

#include <yt/yt/client/table_client/row_buffer.h>
#include <yt/yt/client/table_client/row_batch.h>

#include <yt/yt/ytlib/job_proxy/private.h>

#include <limits>
#include <vector>

namespace NYT::NJobProxy {

using namespace NApi;
using namespace NChunkClient;
using namespace NNodeTrackerClient;
using namespace NObjectClient;
using namespace NControllerAgent;
using namespace NControllerAgent::NProto;
using namespace NTableClient;
using namespace NTransactionClient;
using namespace NYson;
using namespace NYTree;
using namespace NConcurrency;

////////////////////////////////////////////////////////////////////////////////

namespace {

ISchemalessMultiChunkWriterPtr CreateTableWriter(
    NNative::IClientPtr client,
    TTableWriterConfigPtr config,
    TTableWriterOptionsPtr options,
    TString localHostName,
    TChunkListId chunkListId,
    TTransactionId transactionId,
    TTableSchemaPtr tableSchema,
    TMasterTableSchemaId schemaId,
    const TChunkTimestamps& chunkTimestamps,
    TTrafficMeterPtr trafficMeter,
    IThroughputThrottlerPtr throttler,
    const std::optional<TDataSink>& dataSink,
    IChunkWriter::TWriteBlocksOptions writeBlocksOptions)
{
    auto nameTable = New<TNameTable>();
    nameTable->SetEnableColumnNameValidation();

    return CreateSchemalessMultiChunkWriter(
        std::move(config),
        std::move(options),
        std::move(nameTable),
        std::move(tableSchema),
        TLegacyOwningKey(),
        std::move(client),
        std::move(localHostName),
        CellTagFromId(chunkListId),
        transactionId,
        schemaId,
        dataSink,
        std::move(writeBlocksOptions),
        chunkListId,
        chunkTimestamps,
        std::move(trafficMeter),
        std::move(throttler));
}

std::vector<TUnversionedRow> FetchReaderKeyPrefixes(
    ISchemalessMultiChunkReaderPtr reader,
    int keyLength,
    const TRowBufferPtr& buffer)
{
    std::vector<TUnversionedRow> keys;
    while (true) {
        auto batch = reader->Read();
        if (!batch) {
            break;
        }
        auto rows = batch->MaterializeRows();

        if (rows.empty()) {
            // Reader is not ready, wait.
            reader->GetReadyEvent().Get().ThrowOnError();
        }

        for (auto row : rows) {
            if (!keys.empty() && ComparePrefix(row.begin(), keys.back().begin(), keyLength) == 0) {
                continue;
            }

            auto key = buffer->CaptureRow(row.FirstNElements(keyLength));
            keys.push_back(key);
        }
    }

    return keys;
}

TSharedRange<TUnversionedRow> DedupRows(
    const TComparator& comparator,
    const std::vector<std::vector<TUnversionedRow>>& tableKeys,
    TRowBufferPtr rowBuffer)
{
    std::vector<TUnversionedRow> keys;

    ssize_t capacity = 0;
    for (auto key : tableKeys) {
        capacity += std::ssize(key);
    }

    keys.reserve(capacity);

    for (auto key : tableKeys) {
        std::copy(key.begin(), key.end(), back_inserter(keys));
    }

    std::sort(keys.begin(), keys.end(), [&] (TUnversionedRow lhs, TUnversionedRow rhs) {
        return comparator.CompareKeys(TKey::FromRow(lhs), TKey::FromRow(rhs)) < 0;
    });

    keys.erase(std::unique(keys.begin(), keys.end()), keys.end());

    return MakeSharedRange<TUnversionedRow>(keys, std::move(rowBuffer));
}

ISchemalessMultiChunkReaderPtr CreateRegularReader(
    std::vector<TDataSliceDescriptor> dataSliceDescriptors,
    TDataSourceDirectoryPtr dataSourceDirectory,
    TTableReaderOptionsPtr options,
    TTableReaderConfigPtr tableReaderConfig,
    TChunkReaderHostPtr chunkReaderHost,
    bool isParallel,
    TNameTablePtr nameTable,
    const TColumnFilter& columnFilter,
    const TClientChunkReadOptions& chunkReadOptions,
    IMultiReaderMemoryManagerPtr multiReaderMemoryManager,
    std::optional<int> partitionTag = std::nullopt)
{
    auto createReader = isParallel
        ? CreateSchemalessParallelMultiReader
        : CreateSchemalessSequentialMultiReader;
    return createReader(
        tableReaderConfig,
        std::move(options),
        std::move(chunkReaderHost),
        dataSourceDirectory,
        std::move(dataSliceDescriptors),
        /*hintKeyPrefixes*/ std::nullopt,
        std::move(nameTable),
        chunkReadOptions,
        TReaderInterruptionOptions::InterruptibleWithEmptyKey(),
        columnFilter,
        partitionTag,
        multiReaderMemoryManager->CreateMultiReaderMemoryManager(tableReaderConfig->MaxBufferSize));
}

std::optional<i64> GetChunkSpecRowCount(const NChunkClient::NProto::TChunkSpec& chunkSpec) {
    if (chunkSpec.has_row_count_override()) {
        return chunkSpec.row_count_override();
    }
    if (HasProtoExtension<NChunkClient::NProto::TMiscExt>(chunkSpec.chunk_meta().extensions())) {
        const auto& misc = GetProtoExtension<NChunkClient::NProto::TMiscExt>(chunkSpec.chunk_meta().extensions());
        if (misc.has_row_count()) {
            return misc.row_count();
        }
    }
    return std::nullopt;
}

std::optional<i64> GetChunkSpecDataWeight(const NChunkClient::NProto::TChunkSpec& chunkSpec) {
    if (chunkSpec.has_data_weight_override()) {
        return chunkSpec.data_weight_override();
    }
    if (HasProtoExtension<NChunkClient::NProto::TMiscExt>(chunkSpec.chunk_meta().extensions())) {
        const auto& misc = GetProtoExtension<NChunkClient::NProto::TMiscExt>(chunkSpec.chunk_meta().extensions());
        if (misc.has_data_weight()) {
            return misc.data_weight();
        }
    }
    return std::nullopt;
}

IMultiReaderMemoryManagerPtr CreateMultiReaderMemoryManager(i64 totalReaderMemoryLimit)
{
    // Initialize parallel reader memory manager.
    TParallelReaderMemoryManagerOptions parallelReaderMemoryManagerOptions{
        .TotalReservedMemorySize = totalReaderMemoryLimit,
        .MaxInitialReaderReservedMemory = totalReaderMemoryLimit
    };
    return CreateParallelReaderMemoryManager(
        parallelReaderMemoryManagerOptions,
        NChunkClient::TDispatcher::Get()->GetReaderMemoryManagerInvoker()
    );
}

} // namespace

////////////////////////////////////////////////////////////////////////////////

struct TUserJobIOFactoryBase
    : public IUserJobIOFactory
{
    TUserJobIOFactoryBase(
        const TClientChunkReadOptions& chunkReadOptions,
        TChunkReaderHostPtr chunkReaderHost,
        TString localHostName,
        IThroughputThrottlerPtr outBandwidthThrottler)
        : ChunkReadOptions_(chunkReadOptions)
        , ChunkReaderHost_(std::move(chunkReaderHost))
        , LocalHostName_(std::move(localHostName))
        , OutBandwidthThrottler_(std::move(outBandwidthThrottler))
    { }

    ISchemalessMultiChunkWriterPtr CreateWriter(
        NApi::NNative::IClientPtr client,
        TTableWriterConfigPtr config,
        TTableWriterOptionsPtr options,
        TChunkListId chunkListId,
        TTransactionId transactionId,
        TTableSchemaPtr tableSchema,
        TMasterTableSchemaId schemaId,
        const TChunkTimestamps& chunkTimestamps,
        const std::optional<TDataSink>& dataSink,
        IChunkWriter::TWriteBlocksOptions writeBlocksOptions) override
    {
        return CreateTableWriter(
            std::move(client),
            std::move(config),
            std::move(options),
            LocalHostName_,
            chunkListId,
            transactionId,
            std::move(tableSchema),
            schemaId,
            chunkTimestamps,
            // NB: This is ok, since traffic meter is shared between readers and writers.
            ChunkReaderHost_->TrafficMeter,
            OutBandwidthThrottler_,
            dataSink,
            std::move(writeBlocksOptions));
    }

protected:
    const TClientChunkReadOptions ChunkReadOptions_;
    const TChunkReaderHostPtr ChunkReaderHost_;
    const TString LocalHostName_;
    const IThroughputThrottlerPtr OutBandwidthThrottler_;
};

////////////////////////////////////////////////////////////////////////////////

class TMapJobIOFactory
    : public TUserJobIOFactoryBase
{
public:
    TMapJobIOFactory(
        bool useParallelReader,
        const TClientChunkReadOptions& chunkReadOptions,
        TChunkReaderHostPtr chunkReaderHost,
        TString localHostName,
        IThroughputThrottlerPtr outBandwidthThrottler)
        : TUserJobIOFactoryBase(
            chunkReadOptions,
            std::move(chunkReaderHost),
            std::move(localHostName),
            std::move(outBandwidthThrottler))
        , UseParallelReader_(useParallelReader)
    { }

    TCreateUserJobReaderResult CreateReader(
        const IJobSpecHelperPtr& jobSpecHelper,
        TClosure /*onNetworkReleased*/,
        TNameTablePtr nameTable,
        const TColumnFilter& columnFilter) override
    {
        auto multiReaderMemoryManager = CreateMultiReaderMemoryManager(jobSpecHelper->GetJobIOConfig()->TableReader->MaxBufferSize);
        return {
            CreateRegularReader(
                jobSpecHelper->UnpackDataSliceDescriptors(),
                jobSpecHelper->GetDataSourceDirectory(),
                jobSpecHelper->GetTableReaderOptions(),
                jobSpecHelper->GetJobIOConfig()->TableReader,
                ChunkReaderHost_,
                UseParallelReader_,
                std::move(nameTable),
                columnFilter,
                ChunkReadOptions_,
                multiReaderMemoryManager),
            std::nullopt
        };
    }

private:
    const bool UseParallelReader_;
};

////////////////////////////////////////////////////////////////////////////////

class TSortedReduceJobIOFactory
    : public TUserJobIOFactoryBase
{
    struct TSortedReduceJobIOFactoryTag
    { };

public:
    TSortedReduceJobIOFactory(
        bool interruptAtKeyEdge,
        const TClientChunkReadOptions& chunkReadOptions,
        TChunkReaderHostPtr chunkReaderHost,
        TString localHostName,
        IThroughputThrottlerPtr outBandwidthThrottler)
        : TUserJobIOFactoryBase(
            chunkReadOptions,
            std::move(chunkReaderHost),
            std::move(localHostName),
            std::move(outBandwidthThrottler))
        , InterruptAtKeyEdge_(interruptAtKeyEdge)
    { }

    TCreateUserJobReaderResult CreateReader(
        const IJobSpecHelperPtr& jobSpecHelper,
        TClosure /*onNetworkReleased*/,
        TNameTablePtr nameTable,
        const TColumnFilter& columnFilter) override
    {
        auto& Logger = JobProxyClientLogger();
        auto rowBuffer = New<TRowBuffer>(TSortedReduceJobIOFactoryTag());

        YT_VERIFY(nameTable->GetSize() == 0 && columnFilter.IsUniversal());

        const auto& reduceJobSpecExt = jobSpecHelper->GetJobSpec().GetExtension(TReduceJobSpecExt::reduce_job_spec_ext);
        auto keyColumns = FromProto<TKeyColumns>(reduceJobSpecExt.key_columns());
        auto sortColumns = FromProto<TSortColumns>(reduceJobSpecExt.sort_columns());
        auto getTotalReaderMemoryLimit = [] (const IJobSpecHelperPtr& jobSpecHelper) {
            auto readerMemoryLimit = jobSpecHelper->GetJobIOConfig()->TableReader->MaxBufferSize;
            const auto& jobSpecExt = jobSpecHelper->GetJobSpecExt();
            auto readerCount = jobSpecExt.input_table_specs_size() + jobSpecExt.foreign_input_table_specs_size();
            return readerMemoryLimit * readerCount;
        };
        auto multiReaderMemoryManager = CreateMultiReaderMemoryManager(getTotalReaderMemoryLimit(jobSpecHelper));
        std::optional<NChunkClient::NProto::TDataStatistics> preparationDataStatistics;

        // COMPAT(gritukan)
        if (sortColumns.empty()) {
            for (const auto& keyColumn : keyColumns) {
                sortColumns.push_back({keyColumn, ESortOrder::Ascending});
            }
        }

        nameTable = TNameTable::FromSortColumns(sortColumns);
        const auto& jobSpecExt = jobSpecHelper->GetJobSpecExt();
        auto dataSourceDirectory = jobSpecHelper->GetDataSourceDirectory();

        // COMPAT(max42, onionalex): remove after all CAs are 22.2+.
        for (auto& dataSource : dataSourceDirectory->DataSources()) {
            if (!dataSource.Schema() || dataSource.Schema()->Columns().empty()) {
                dataSource.Schema() = TTableSchema::FromSortColumns(sortColumns);
            }
        }

        if (reduceJobSpecExt.disable_sorted_input() && jobSpecExt.foreign_input_table_specs_size() == 0) {
            // Input tables are currently sorted, although this property is not utilized by this reader.
            // Intermediate sorting is necessary to distribute chunks among sorted reduce jobs
            // in the current implementation.

            return {
                CreateRegularReader(
                    jobSpecHelper->UnpackDataSliceDescriptors(),
                    jobSpecHelper->GetDataSourceDirectory(),
                    jobSpecHelper->GetTableReaderOptions(),
                    jobSpecHelper->GetJobIOConfig()->TableReader,
                    ChunkReaderHost_,
                    /*isParallel*/ true,
                    std::move(nameTable),
                    columnFilter,
                    ChunkReadOptions_,
                    multiReaderMemoryManager),
                preparationDataStatistics
            };
        }

        auto options = ConvertTo<TTableReaderOptionsPtr>(TYsonString(
            jobSpecExt.table_reader_options()));

        // We must always enable table index to merge rows with the same index in the proper order.
        options->EnableTableIndex = true;

        // We must always enable key widening to prevent out of range access of key prefixes in sorted merging/joining readers.
        options->EnableKeyWidening = true;

        // If the primary table is small, read it out completely into the memory to obtain
        // join keys.
        i64 inputRowCount = 0;
        i64 inputDataWeight = 0;
        for (const auto& inputSpec : jobSpecExt.input_table_specs()) {
            for (const auto& chunkSpec : inputSpec.chunk_specs()) {
                auto chunkSpecRowCount = GetChunkSpecRowCount(chunkSpec);
                if (!chunkSpecRowCount) {
                    // No estimate possible.
                    inputRowCount = std::numeric_limits<decltype(inputRowCount)>::max();
                    break;
                }
                inputRowCount += *chunkSpecRowCount;
                auto chunkSpecDataWeight = GetChunkSpecDataWeight(chunkSpec);
                if (!chunkSpecDataWeight) {
                    inputDataWeight = std::numeric_limits<decltype(inputDataWeight)>::max();
                    break;
                }
                inputDataWeight += *chunkSpecDataWeight;
            }
            if (inputRowCount == std::numeric_limits<decltype(inputRowCount)>::max() ||
                inputDataWeight == std::numeric_limits<decltype(inputDataWeight)>::max()) {
                break;
            }
        }

        std::vector<std::vector<TUnversionedRow>> primaryKeyPrefixes;
        std::optional<THintKeyPrefixes> hintKeyPrefixes;

        int foreignKeyColumnCount = reduceJobSpecExt.join_key_column_count();
        auto sortComparator = GetComparator(sortColumns);
        auto reduceComparator = sortComparator.Trim(reduceJobSpecExt.reduce_key_column_count());
        auto joinComparator = sortComparator.Trim(foreignKeyColumnCount);

        if (reduceJobSpecExt.has_foreign_table_lookup_keys_threshold() &&
            inputRowCount < reduceJobSpecExt.foreign_table_lookup_keys_threshold() &&
            inputDataWeight < reduceJobSpecExt.foreign_table_lookup_data_weight_threshold() &&
            jobSpecExt.foreign_input_table_specsSize() > 0)
        {
            primaryKeyPrefixes.resize(jobSpecExt.input_table_specs_size());
            i64 primaryRowCount = 0;
            // TODO(orlovorlov): surface it in `yt get-job` output that a preliminary
            // pass was performed.

            YT_LOG_INFO(
                "Starting preliminary pass to read all keys from primary table (ForeignTableCount: %v)",
                jobSpecExt.foreign_input_table_specsSize());

            NChunkClient::NProto::TDataStatistics statistics;
            for (int i = 0; i < jobSpecExt.input_table_specs_size(); i++) {
                const auto& inputSpec = jobSpecExt.input_table_specs(i);
                auto dataSliceDescriptors = UnpackDataSliceDescriptors(inputSpec);
                const auto& tableReaderConfig = jobSpecHelper->GetJobIOConfig()->TableReader;
                auto memoryManager = multiReaderMemoryManager->CreateMultiReaderMemoryManager(tableReaderConfig->MaxBufferSize);

                // TODO(orlovorlov) YT-18240: only read key columns here.
                auto reader = CreateSchemalessSequentialMultiReader(
                    tableReaderConfig,
                    options,
                    ChunkReaderHost_,
                    dataSourceDirectory,
                    std::move(dataSliceDescriptors),
                    /*hintKeyPrefixes*/ std::nullopt,
                    nameTable,
                    ChunkReadOptions_,
                    TReaderInterruptionOptions::InterruptibleWithKeyLength(std::ssize(sortColumns)),
                    columnFilter,
                    /*partitionTag*/ std::nullopt,
                    memoryManager);

                primaryKeyPrefixes[i] = FetchReaderKeyPrefixes(reader, reduceJobSpecExt.join_key_column_count(), rowBuffer);
                statistics += reader->GetDataStatistics();
                primaryRowCount += std::ssize(primaryKeyPrefixes[i]);
            }
            hintKeyPrefixes = THintKeyPrefixes(DedupRows(joinComparator, primaryKeyPrefixes, rowBuffer));

            YT_LOG_INFO(
                "Read all keys from primary table in a preliminary pass "
                "(EstimatedRowCount: %v, ActualRowCount: %v, DedupedRowCount: %v, "
                "NumForeignTables: %v)",
                inputRowCount, primaryRowCount, std::ssize(hintKeyPrefixes->HintPrefixes),
                jobSpecExt.foreign_input_table_specsSize());

            if (preparationDataStatistics) {
                *preparationDataStatistics += statistics;
            } else {
                preparationDataStatistics = statistics;
            }
        }

        std::vector<ISchemalessMultiChunkReaderPtr> primaryReaders;
        for (const auto& inputSpec : jobSpecExt.input_table_specs()) {
            // ToDo(psushin): validate that input chunks are sorted.
            auto dataSliceDescriptors = UnpackDataSliceDescriptors(inputSpec);
            const auto& tableReaderConfig = jobSpecHelper->GetJobIOConfig()->TableReader;
            auto memoryManager = multiReaderMemoryManager->CreateMultiReaderMemoryManager(tableReaderConfig->MaxBufferSize);

            auto reader = CreateSchemalessSequentialMultiReader(
                tableReaderConfig,
                options,
                ChunkReaderHost_,
                dataSourceDirectory,
                std::move(dataSliceDescriptors),
                /*hintKeyPrefixes*/ std::nullopt,
                nameTable,
                ChunkReadOptions_,
                TReaderInterruptionOptions::InterruptibleWithKeyLength(std::ssize(sortColumns)),
                columnFilter,
                /*partitionTag*/ std::nullopt,
                memoryManager);

            primaryReaders.emplace_back(reader);
        }

        std::vector<ISchemalessMultiChunkReaderPtr> foreignReaders;

        for (const auto& inputSpec : jobSpecExt.foreign_input_table_specs()) {
            auto dataSliceDescriptors = UnpackDataSliceDescriptors(inputSpec);

            const auto& tableReaderConfig = jobSpecHelper->GetJobIOConfig()->TableReader;

            auto reader = CreateSchemalessSequentialMultiReader(
                tableReaderConfig,
                options,
                ChunkReaderHost_,
                dataSourceDirectory,
                std::move(dataSliceDescriptors),
                hintKeyPrefixes,
                nameTable,
                ChunkReadOptions_,
                TReaderInterruptionOptions::NonInterruptible(),
                columnFilter,
                /*partitionTag*/ std::nullopt,
                multiReaderMemoryManager->CreateMultiReaderMemoryManager(tableReaderConfig->MaxBufferSize));

            foreignReaders.emplace_back(reader);
        }

        return {
            CreateSortedJoiningReader(
                primaryReaders,
                sortComparator,
                reduceComparator,
                foreignReaders,
                joinComparator,
                InterruptAtKeyEdge_),
            preparationDataStatistics
        };
    }

private:
    const bool InterruptAtKeyEdge_;
};

////////////////////////////////////////////////////////////////////////////////

class TPartitionMapJobIOFactory
    : public TUserJobIOFactoryBase
{
public:
    explicit TPartitionMapJobIOFactory(
        const IJobSpecHelperPtr& jobSpecHelper,
        const TClientChunkReadOptions& chunkReadOptions,
        TChunkReaderHostPtr chunkReaderHost,
        TString localHostName,
        IThroughputThrottlerPtr outBandwidthThrottler)
        : TUserJobIOFactoryBase(
            chunkReadOptions,
            std::move(chunkReaderHost),
            std::move(localHostName),
            std::move(outBandwidthThrottler))
        , PartitionJobSpecExt_(jobSpecHelper->GetJobSpec().GetExtension(TPartitionJobSpecExt::partition_job_spec_ext))
    { }

    TCreateUserJobReaderResult CreateReader(
        const IJobSpecHelperPtr& jobSpecHelper,
        TClosure /*onNetworkReleased*/,
        TNameTablePtr nameTable,
        const TColumnFilter& columnFilter) override
    {
        auto multiReaderMemoryManager = CreateMultiReaderMemoryManager(jobSpecHelper->GetJobIOConfig()->TableReader->MaxBufferSize);
        return {
            CreateRegularReader(
                jobSpecHelper->UnpackDataSliceDescriptors(),
                jobSpecHelper->GetDataSourceDirectory(),
                jobSpecHelper->GetTableReaderOptions(),
                jobSpecHelper->GetJobIOConfig()->TableReader,
                ChunkReaderHost_,
                /*isParallel*/ !PartitionJobSpecExt_.use_sequential_reader(),
                std::move(nameTable),
                columnFilter,
                ChunkReadOptions_,
                multiReaderMemoryManager),
            std::nullopt
        };
    }

    ISchemalessMultiChunkWriterPtr CreateWriter(
        NNative::IClientPtr client,
        TTableWriterConfigPtr config,
        TTableWriterOptionsPtr options,
        TChunkListId chunkListId,
        TTransactionId transactionId,
        TTableSchemaPtr tableSchema,
        TMasterTableSchemaId schemaId,
        const TChunkTimestamps& chunkTimestamps,
        const std::optional<TDataSink>& dataSink,
        IChunkWriter::TWriteBlocksOptions writeBlocksOptions) override
    {
        auto partitioner = CreatePartitioner(PartitionJobSpecExt_);

        // We pass partitioning columns through schema but input stream is not sorted.
        options->ValidateSorted = false;

        // TODO(max42): currently ReturnBoundaryKeys are set exactly for the writers
        // that correspond to the map-sink edge. Think more about how this may be done properly.
        if (!options->ReturnBoundaryKeys) {
            auto keyColumns = FromProto<TKeyColumns>(PartitionJobSpecExt_.sort_key_columns());
            auto sortColumns = FromProto<TSortColumns>(PartitionJobSpecExt_.sort_columns());
            // COMPAT(gritukan)
            if (sortColumns.empty()) {
                for (const auto& keyColumn : keyColumns) {
                    sortColumns.push_back(TColumnSortSchema{
                        .Name = keyColumn,
                        .SortOrder = ESortOrder::Ascending
                    });
                }
            }

            auto nameTable = TNameTable::FromKeyColumns(keyColumns);
            nameTable->SetEnableColumnNameValidation();
            if (tableSchema->Columns().empty()) {
                tableSchema = TTableSchema::FromSortColumns(sortColumns);
            }

            // This writer is used for partitioning.
            return CreatePartitionMultiChunkWriter(
                std::move(config),
                std::move(options),
                std::move(nameTable),
                std::move(tableSchema),
                std::move(client),
                LocalHostName_,
                CellTagFromId(chunkListId),
                transactionId,
                schemaId,
                chunkListId,
                std::move(partitioner),
                dataSink,
                std::move(writeBlocksOptions),
                ChunkReaderHost_->TrafficMeter,
                OutBandwidthThrottler_);
        } else {
            // This writer is used for mapper output tables.
            return CreateTableWriter(
                std::move(client),
                std::move(config),
                std::move(options),
                LocalHostName_,
                chunkListId,
                transactionId,
                std::move(tableSchema),
                schemaId,
                chunkTimestamps,
                ChunkReaderHost_->TrafficMeter,
                OutBandwidthThrottler_,
                dataSink,
                std::move(writeBlocksOptions));
        }
    }

private:
    TPartitionJobSpecExt PartitionJobSpecExt_;
};

////////////////////////////////////////////////////////////////////////////////

class TPartitionReduceJobIOFactory
    : public TUserJobIOFactoryBase
{
public:
    TPartitionReduceJobIOFactory(
        const TClientChunkReadOptions& chunkReadOptions,
        TChunkReaderHostPtr chunkReaderHost,
        TString localHostName,
        IThroughputThrottlerPtr outBandwidthThrottler)
        : TUserJobIOFactoryBase(
            chunkReadOptions,
            std::move(chunkReaderHost),
            std::move(localHostName),
            std::move(outBandwidthThrottler))
    { }

    TCreateUserJobReaderResult CreateReader(
        const IJobSpecHelperPtr& jobSpecHelper,
        TClosure onNetworkReleased,
        TNameTablePtr nameTable,
        const TColumnFilter& columnFilter) override
    {
        YT_VERIFY(nameTable->GetSize() == 0 && columnFilter.IsUniversal());

        const auto& jobSpecExt = jobSpecHelper->GetJobSpecExt();

        YT_VERIFY(jobSpecExt.input_table_specs_size() == 1);

        const auto& inputSpec = jobSpecExt.input_table_specs(0);
        auto dataSliceDescriptors = UnpackDataSliceDescriptors(inputSpec);
        auto dataSourceDirectory = jobSpecHelper->GetDataSourceDirectory();

        const auto& reduceJobSpecExt = jobSpecHelper->GetJobSpec().GetExtension(TReduceJobSpecExt::reduce_job_spec_ext);
        auto keyColumns = FromProto<TKeyColumns>(reduceJobSpecExt.key_columns());
        auto sortColumns = FromProto<TSortColumns>(reduceJobSpecExt.sort_columns());

        // COMPAT(gritukan)
        if (sortColumns.empty()) {
            for (const auto& keyColumn : keyColumns) {
                sortColumns.push_back({keyColumn, ESortOrder::Ascending});
            }
        }

        nameTable = TNameTable::FromKeyColumns(keyColumns);

        std::optional<int> partitionTag;
        if (jobSpecExt.has_partition_tag()) {
            partitionTag = jobSpecExt.partition_tag();
        } else if (reduceJobSpecExt.has_partition_tag()) {
            partitionTag = reduceJobSpecExt.partition_tag();
        }
        YT_VERIFY(partitionTag);

        auto multiReaderMemoryManager = CreateMultiReaderMemoryManager(jobSpecHelper->GetJobIOConfig()->TableReader->MaxBufferSize);

        if (reduceJobSpecExt.disable_sorted_input()) {
            return {
                CreateRegularReader(
                    jobSpecHelper->UnpackDataSliceDescriptors(),
                    jobSpecHelper->GetDataSourceDirectory(),
                    jobSpecHelper->GetTableReaderOptions(),
                    jobSpecHelper->GetJobIOConfig()->TableReader,
                    ChunkReaderHost_,
                    /*isParallel*/ true,
                    std::move(nameTable),
                    columnFilter,
                    ChunkReadOptions_,
                    multiReaderMemoryManager,
                    partitionTag),
                std::nullopt
            };
        }

        return {
            CreatePartitionSortReader(
                jobSpecHelper->GetJobIOConfig()->TableReader,
                ChunkReaderHost_,
                GetComparator(sortColumns),
                nameTable,
                onNetworkReleased,
                dataSourceDirectory,
                std::move(dataSliceDescriptors),
                jobSpecExt.input_row_count(),
                jobSpecExt.is_approximate(),
                *partitionTag,
                ChunkReadOptions_,
                multiReaderMemoryManager),
            std::nullopt
        };
    }
};

////////////////////////////////////////////////////////////////////////////////

class TVanillaJobIOFactory
    : public TUserJobIOFactoryBase
{
public:
    TVanillaJobIOFactory(
        const TClientChunkReadOptions& chunkReadOptions,
        TChunkReaderHostPtr chunkReaderHost,
        TString localHostName,
        IThroughputThrottlerPtr outBandwidthThrottler)
        : TUserJobIOFactoryBase(
            chunkReadOptions,
            std::move(chunkReaderHost),
            std::move(localHostName),
            std::move(outBandwidthThrottler))
    { }

    TCreateUserJobReaderResult CreateReader(
        const IJobSpecHelperPtr& /*jobSpecHelper*/,
        TClosure /*onNetworkReleased*/,
        TNameTablePtr /*nameTable*/,
        const TColumnFilter& /*columnFilter*/) override
    {
        return {};
    }
};

////////////////////////////////////////////////////////////////////////////////

IUserJobIOFactoryPtr CreateUserJobIOFactory(
    const IJobSpecHelperPtr& jobSpecHelper,
    const TClientChunkReadOptions& chunkReadOptions,
    TChunkReaderHostPtr chunkReaderHost,
    TString localHostName,
    IThroughputThrottlerPtr outBandwidthThrottler)
{
    const auto jobType = jobSpecHelper->GetJobType();
    switch (jobType) {
        case EJobType::Map:
            return New<TMapJobIOFactory>(
                true,
                chunkReadOptions,
                std::move(chunkReaderHost),
                std::move(localHostName),
                std::move(outBandwidthThrottler));

        case EJobType::OrderedMap:
            return New<TMapJobIOFactory>(
                false,
                chunkReadOptions,
                std::move(chunkReaderHost),
                std::move(localHostName),
                std::move(outBandwidthThrottler));

        case EJobType::SortedReduce:
            return New<TSortedReduceJobIOFactory>(
                true,
                chunkReadOptions,
                std::move(chunkReaderHost),
                std::move(localHostName),
                std::move(outBandwidthThrottler));

        case EJobType::JoinReduce:
            return New<TSortedReduceJobIOFactory>(
                false,
                chunkReadOptions,
                std::move(chunkReaderHost),
                std::move(localHostName),
                std::move(outBandwidthThrottler));

        case EJobType::PartitionMap:
            return New<TPartitionMapJobIOFactory>(
                jobSpecHelper,
                chunkReadOptions,
                std::move(chunkReaderHost),
                std::move(localHostName),
                std::move(outBandwidthThrottler));

        // ToDo(psushin): handle separately to form job result differently.
        case EJobType::ReduceCombiner:
        case EJobType::PartitionReduce:
            return New<TPartitionReduceJobIOFactory>(
                chunkReadOptions,
                std::move(chunkReaderHost),
                std::move(localHostName),
                std::move(outBandwidthThrottler));

        case EJobType::Vanilla:
            return New<TVanillaJobIOFactory>(
                chunkReadOptions,
                std::move(chunkReaderHost),
                std::move(localHostName),
                std::move(outBandwidthThrottler));

        default:
            THROW_ERROR_EXCEPTION(
                "Job has an invalid type %Qlv while a user job is expected",
                jobType);
    }
}

////////////////////////////////////////////////////////////////////////////////

} // namespace NYT::NJobProxy
