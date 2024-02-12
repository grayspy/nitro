# List of test vectors ids that can be found in tests
## Withdrawals tests EIP-4895

- ID: **BSHA0001** check that Shanghai block rejected before transition
- ID: **BSHA0002** check that Merge block rejected after transition
- ID: **BSHA0003** check that Block without withdrawals root header field but with withdrawals in the body gets rejected after transition
- ID: **BSHA0004** check that if withdrawals root does not match the withdrawals body block rejected
- ID: **BSHA0005** check that Shanghai block without withdrawals rlp body is rejected as incomplete
- ID: **BSHA0006** check if withdrawals rlp body is not presented as List

### Withdrawals elements bounds
- ID: **BSHB0001** withdrawals index is 2^64-1
- ID: **BSHB0002** withdrawals index is 2^64
- ID: **BSHB0003** withdrawals index is bigint
- ID: **BSHB0004** withdrawals validatorIndex 2^64-1
- ID: **BSHB0005** withdrawals validatorIndex 2^64
- ID: **BSHB0006** withdrawals validatorIndex bigint
- ID: **BSHB0007** withdrawals address is 0x0000..00
- ID: **BSHB0008** withdrawals address is less than 20 bytes
- ID: **BSHB0009** withdrawals address is more than 20 bytes
- ID: **BSHB0010** withdrawals amount 0 is fine
- ID: **BSHB0011** withdrawals amount more 2^256 is bad
- ID: **BSHB0012** withdrawals amount more 2^64 is ok
- ID: **BSHB0013** withdrawals body has more than 4 elements
- ID: **BSHB0014** withdrawals body has less than 4 elements
- ID: **BSHB0015** two withdrawals record with the same index  (WARNING: ALLOWED)
- ID: **BSHB0016** different validators withdraw to the same address
- ID: **BSHB0017** withdrawal amount is 0 (WARNING: touch empty account)
- ID: **BSHB0018** withdrawal amount is 0, plus transaction



## EOF1 contracts

### Invalid examples

- ID: **EOF1I0001** check that EOF1 with a bad magic number fails
- ID: **EOF1I0002** check that EOF1 with a bad version number fails
- ID: **EOF1I0003** check that EOF1 with a bad section order fails
- ID: **EOF1I0004** check that EOF1 missing a section fails
- ID: **EOF1I0005** check that EOF1 with a bad end of sections number fails
- ID: **EOF1I0006** check that EOF1 with too many or too few bytes fails
- ID: **EOF1I0007** check that EOF1 with a malformed code section fails
- ID: **EOF1I0008** check that EOF1 with an illegal opcode fails
- ID: **EOF1I0009** check that EOF1 with the wrong maxStackDepth fails
- ID: **EOF1I0010** check that return values are not allowed on section 0
- ID: **EOF1I0011** check that function calls to code sections that don't exist fail
- ID: **EOF1I0012** check that code sections that cause stack underflow fail
- ID: **EOF1I0013** check that we can't return more values than we declare
- ID: **EOF1I0014** check that code that looks deeper in the stack than the parameters fails
- ID: **EOF1I0015** check that code that uses removed opcodes fails
- ID: **EOF1I0016** check that code that uses new relative jumps to outside the section fails
- ID: **EOF1I0017** check that parameters are not allowed on section 0
- ID: **EOF1I0018** inconsistent number of code sections (between types and code)
- ID: **EOF1I0019** check that jumps into the middle on an opcode are not allowed
- ID: **EOF1I0020** check that you can't get to the same opcode with two different stack heights
- ID: **EOF1I0021** empty jump table
- ID: **EOF1I0022** stack underflow caused by a function call
- ID: **EOF1I0023** sections with unreachable code fail
- ID: **EOF1I0024** sections that end with a non-terminator opcode fail
- ID: **EOF1I0025** data stack height of 1024 is invalid


### Valid examples

- ID: **EOF1V0001** check that simple valid EOF1 deploys
- ID: **EOF1V0002** check that valid EOF1 with two code sections deploys
- ID: **EOF1V0003** check that valid EOF1 with four code sections deploys
- ID: **EOF1V0004** check that valid EOF1 can include 0xFE, the designated "invalid opcode"
- ID: **EOF1V0005** check that EOF1 with the right maxStackDepth deploys
- ID: **EOF1V0006** check that return values are allowed on code sections that aren't zero
- ID: **EOF1V0007** check that function calls to code sections that exist are allowed
- ID: **EOF1V0008** check that code that uses a new style relative jump (5C) succeeds
- ID: **EOF1V0009** check that parameters are allowed on code sections that aren't zero
- ID: **EOF1V0010** parameters are part of the max stack height
- ID: **EOF1V0011** check that code that uses a new style conditional jump (5D) succeeds
- ID: **EOF1V0012** return values on code sections affect maxStackHeight of the caller
- ID: **EOF1V0013** jump tables work
- ID: **EOF1V0014** sections that end with a legit terminating opcode are OK
- ID: **EOF1V0015** data stack height of 1023 is valid


### Contract creation tests

- ID: **EOF1C0001** legacy contract > CREATE > legacy init code > legacy deploy code
- ID: **EOF1C0002** legacy contract > CREATE > legacy init code > eof1 deploy code
- ID: **EOF1C0003** legacy contract > CREATE > eof1 init code > legacy deploy code (fail)
- ID: **EOF1C0004** legacy contract > CREATE > eof1 init code > eof1 deploy code
- ID: **EOF1C0005** legacy contract > CREATE2 > legacy init code > legacy deploy code
- ID: **EOF1C0006** legacy contract > CREATE2 > legacy init code > eof1 deploy code
- ID: **EOF1C0007** legacy contract > CREATE2 > eof1 init code > legacy deploy code (fail)
- ID: **EOF1C0008** legacy contract > CREATE2 > eof1 init code > eof1 deploy code
- ID: **EOF1C0009** eof1 contract > CREATE > legacy init code > legacy deploy code (fail)
- ID: **EOF1C0010** eof1 contract > CREATE > legacy init code > eof1 deploy code (fail)
- ID: **EOF1C0011** eof1 contract > CREATE > eof1 init code > legacy deploy code (fail)
- ID: **EOF1C0012** eof1 contract > CREATE > eof1 init code > eof1 deploy code
- ID: **EOF1C0013** eof1 contract > CREATE2 > legacy init code > legacy deploy code (fail)
- ID: **EOF1C0014** eof1 contract > CREATE2 > legacy init code > eof1 deploy code (fail)
- ID: **EOF1C0015** eof1 contract > CREATE2 > eof1 init code > legacy deploy code (fail)
- ID: **EOF1C0016** eof1 contract > CREATE2 > eof1 init code > eof1 deploy code
- ID: **EOF1C0017** legacy contact > CREATE[2] > legacy init code > nonsense
- ID: **EOF1C0018** legacy contact > CREATE[2] > eof1 init code > nonsense (fail)
- ID: **EOF1C0019** legacy contact > CREATE[2] > nonsense init code (fail)
