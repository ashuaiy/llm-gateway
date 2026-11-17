-- Validate types before any writes: Redis scripts do not roll back on error.
local expected = {'hash','hash','hash','zset'}
local clock=redis.call('TIME');local now=clock[1]*1000+math.floor(clock[2]/1000)
for i=1,4 do
  local kind=redis.call('TYPE',KEYS[i]).ok
  if kind~='none' and kind~=expected[i] then return -3 end
end
local existing=redis.call('HGET',KEYS[2],'state')
if existing then
  if existing=='reserved' and redis.call('HGET',KEYS[2],'reservation')==ARGV[5] then return 1 end
  return -1
end
local spent=tonumber(redis.call('HGET',KEYS[1],'spent'))
local held=tonumber(redis.call('HGET',KEYS[1],'held'))
if not spent or not held then
  if redis.call('HEXISTS',KEYS[3],ARGV[1])==1 or redis.call('EXISTS',KEYS[1])==1 then return -2 end
  spent=tonumber(ARGV[3]); held=0
end
local limit=tonumber(ARGV[2]); local amount=tonumber(ARGV[4]); local max=tonumber(ARGV[8])
if spent<0 or held<0 or spent>max or held>max-spent or amount>max-spent-held then return -3 end
if limit>0 and amount>limit-spent-held then return 0 end
redis.call('HSET',KEYS[1],'spent',string.format('%.0f',spent),'held',string.format('%.0f',held+amount))
redis.call('HSET',KEYS[3],ARGV[1],'1')
redis.call('HSET',KEYS[2],'state','reserved','reservation',ARGV[5],'amount',ARGV[4],'created_millis',now)
redis.call('ZADD',KEYS[4],now,ARGV[7])
return 1
