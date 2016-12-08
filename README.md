# boa

Boa was initially written as a service for [Skyris](https://skyris.co) that converted music uploaded to S3 into smaller streamable MP3s (128kbps), as well as converted lossless uploads to 320kbps MP3s to make them available for download if a user so desired. It depends on the [LAME](http://lame.sourceforge.net/) MP3 encoder. What you'll find here is a cleaned up, slightly more generalized version of this service.

It works in a few vaguely snakelike stages:

1. **Stalk** - read messages off queue
2. **Hunt** - download file included in message from S3
3. **Constrict** - compresses file (if necessary) into the desired MP3s
4. **Digest** - upload newly created files to S3, delete local copies

![snake-boy](https://i.imgur.com/gJj0HIS.gif)

## To Do:

0. CLI config
1. Clean up error handling
2. Add support for other messaging queues and storage (MongoDB & RabbitMQ currently supported)
3. Dockerize so installing the LAME dependency isn't an issue
4. Write tests, set up CI
