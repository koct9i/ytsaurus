RECURSE(
    map_perf
    run
)

IF (NOT OPENSOURCE)
    RECURSE(
        analyze
        compare
    )
ENDIF()
