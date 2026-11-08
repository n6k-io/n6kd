# Security results

Run on 2026-11-08, with `--perms` and `--lockdown`. ➖ means not applicable.

| | n6k | quack |
| --- | --- | --- |
| ✅ holds | 17 | 14 |
| ❌ does not hold | 0 | 2 |
| 💥 check could not run | 0 | 0 |

## Perms (vcatbridge)

| check | n6k | quack |
| --- | --- | --- |
| Row rule holds over attach | ✅ | ✅ |
| Row rule holds through raw SQL | ✅ | ✅ |
| A filter can't reach hidden rows | ✅ | ✅ |
| Row rule holds in a join and an aggregate | ✅ | ✅ |
| Writes denied on a select-only table, over attach | ✅ | ✅ |
| Writes denied on a select-only table, through raw SQL | ✅ | ✅ |
| Unlisted table unreachable | ✅ | ✅ |

## Lockdown

| check | n6k | quack |
| --- | --- | --- |
| File read denied | ✅ | ✅ |
| File write denied | ✅ | ✅ |
| LOAD and INSTALL have no effect | ✅ | ✅ |
| Changing a locked setting has no effect | ✅ | ✅ |

## Session isolation

| check | n6k | quack |
| --- | --- | --- |
| Table created by one client is hidden from others | ✅ | ❌ |
| Database attached by one client is hidden from others | ✅ | ❌ |
| SET can't change another client's limits | ✅ | ✅ |

## Identity

| check | n6k | quack |
| --- | --- | --- |
| Missing credential rejected | ✅ | ✅ |
| Wrong credential rejected | ✅ | ✅ |
| Rows differ by JWT claim | ✅ | ➖ |

## Details

- ❌ **quack, Table created by one client is hidden from others:** got `([(1,)], [(1,)])`, expected `([(1,)], [(0,)])`
- ❌ **quack, Database attached by one client is hidden from others:** got `([(1,)], [(1,)])`, expected `([(1,)], [(0,)])`
