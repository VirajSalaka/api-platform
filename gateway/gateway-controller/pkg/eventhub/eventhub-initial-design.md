Prompt plan for claude.

Inside sync package we have used the concept of states and events. And for each update we made the states and events table update atomic. Now we are going to remove the atomicity. You can use sync package for referencing how to implement. But do not import anything from sync package as I will refactor that later on.

This eventhub can act as a separate simple message broker implementation based on EventHub interface.

It has following methods. (Method signature can be changed, added some parameters to help modeling)

Initialize()
PublishEvent()
RegisterTopic(TopicName, QueueSize)
RegisterSubscription(TopicName, Channel of Events) - Channel would contain an array of items
CleanUpEvents(TimeFrom, TimeEnd)

initialize method is where we create DB connections. 
on RegisterTopic do not create any table. Rather check for that particular table name and return error if it does not exist. If it exists create a queue to record events. Underlying, it is a queue. In addition, this should create entries in States Table with empty version.
When publish event is triggered, it would update the database states and events table.
On RegisterSubscription, this would keep the provided channel mapped to the topic Name. From the point of Subscription, this would have add the events to the channel attached. We process the events as a batch hence an array.
CleanupEvents would delete data past 1 hour to reduce the table growing.

Here is how config deployment works after this implementation. Let's not implement this now. provided for reference on how it would used. Only stick into eventhub implementation.

API-Update happens in Database upon REST API Request. 
CallEventHub.publish
Send the success response to the client.
processAPIEvent would consume the channel used to subscribe and continue on. 

I need step by step implementation as a human engineer do to ease reviewing.
